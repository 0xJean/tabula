//go:build integration

package pgxdriver_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/stdsql"
)

type pgxTraceRec struct {
	mu     sync.Mutex
	events []pgxTraceEvent
}

type pgxTraceEvent struct {
	info tabula.TraceInfo
	rows int64
	err  error
}

func (r *pgxTraceRec) Start(ctx context.Context, info tabula.TraceInfo) (context.Context, func(int64, error)) {
	return ctx, func(rows int64, err error) {
		r.mu.Lock()
		r.events = append(r.events, pgxTraceEvent{info: info, rows: rows, err: err})
		r.mu.Unlock()
	}
}

func (r *pgxTraceRec) snapshot() []pgxTraceEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]pgxTraceEvent, len(r.events))
	copy(out, r.events)
	return out
}

// The caller holds an open cursor. Timing out unwinds the iteration so a
// regression can release the blocked operation and finish the test.
func requireSessionBusy(t *testing.T, op func() error) {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- op() }()
	select {
	case err := <-result:
		require.ErrorIs(t, err, tabula.ErrSessionBusy)
	case <-time.After(3 * time.Second):
		t.Fatal("operation waited for the active row cursor")
	}
}

func TestV031PgxRoleRunnerSharedSealedCopyAndCrossDriverNesting(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	schema := "tabula_v031_pgx_" + suffix
	roleName := "tabula_pgx_role_" + suffix

	_, err = pool.Exec(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })

	_, err = pool.Exec(ctx, `CREATE ROLE `+roleName+` NOLOGIN`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP ROLE IF EXISTS `+roleName) })

	_, err = pool.Exec(ctx, `CREATE TABLE `+schema+`.items (
		id bigserial PRIMARY KEY,
		tenant_id text NOT NULL,
		label text NOT NULL,
		secret_token text NOT NULL DEFAULT ''
	)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `GRANT USAGE ON SCHEMA `+schema+` TO `+roleName)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA `+schema+` TO `+roleName)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA `+schema+` TO `+roleName)
	require.NoError(t, err)

	type scope string
	type item struct {
		ID          int64  `db:"id,key,serial"`
		Tenant      scope  `db:"tenant_id,scope"`
		Label       string `db:"label"`
		SecretToken string `db:"secret_token,secret"`
	}
	type ctxKey struct{}

	key := tabula.NewSessionKey()
	regTrace := &pgxTraceRec{}
	reg := tabula.NewRegistry(tabula.RegistryConfig[scope]{
		RequireSealed: true,
		ScopeID:       func(s scope) any { return string(s) },
		Tracer:        regTrace,
	})
	tbl := tabula.Define(reg, tabula.TableSpec[item, int64]{Name: schema + ".items", Order: "id ASC"})
	require.NoError(t, reg.Seal(key))

	// Registry denying CopyFrom and unscoped writes (A6).
	denyReg := tabula.NewRegistry(tabula.RegistryConfig[scope]{
		SessionKey: key,
		Deny:       tabula.DenyCopy | tabula.DenyUnscopedWrites,
		ScopeID:    func(s scope) any { return string(s) },
	})
	denyTbl := tabula.Define(denyReg, tabula.TableSpec[item, int64]{Name: schema + ".items", Order: "id ASC"})

	unscopedDenyReg := tabula.NewRegistry(tabula.RegistryConfig[scope]{
		SessionKey: key,
		Deny:       tabula.DenyUnscopedWrites,
		ScopeID:    func(s scope) any { return string(s) },
	})
	unscopedDenyTbl := tabula.Define(unscopedDenyReg, tabula.TableSpec[item, int64]{Name: schema + ".items", Order: "id ASC"})

	runnerTrace := &pgxTraceRec{}
	var runnerLogBuf bytes.Buffer
	policy := pgxdriver.ScopePolicy[scope]{
		ManagedVars: []string{"app.scope", "app.mode", "app.req_id"},
		ScopeVars: func(s scope) map[string]string {
			return map[string]string{"app.scope": string(s), "app.mode": "scoped"}
		},
		ContextVars: func(ctx context.Context) map[string]string {
			v, _ := ctx.Value(ctxKey{}).(string)
			return map[string]string{"app.req_id": v}
		},
		IsUnscoped:   func(s scope) bool { return s == "all" },
		UnscopedVars: map[string]string{"app.mode": "unscoped"},
		InactiveVars: map[string]string{"app.scope": "", "app.mode": "inactive"},
		ScopeLog:     func(s scope) string { return "tenant:" + string(s) },
	}
	runner, err := pgxdriver.NewRunner(pool, policy, pgxdriver.RunnerOptions{
		Role:          roleName,
		RequireRole:   true,
		Unscoped:      pgxdriver.UnscopedReadWrite,
		SessionKey:    key,
		Tracer:        runnerTrace,
		TagStatements: true,
		Log: tabula.LogConfig{
			Logger: slog.New(slog.NewTextHandler(&runnerLogBuf, nil)),
			Level:  tabula.LogAll,
			Values: tabula.ValuesAllowed,
			SQL:    true,
		},
		Timeouts: pgxdriver.Timeouts{
			Lock:              2 * time.Second,
			Statement:         5 * time.Second,
			IdleInTransaction: 10 * time.Second,
			Transaction:       15 * time.Second,
		},
	})
	require.NoError(t, err)

	// Separate stdsql runner to verify cross-module ErrNested detection (A1).
	stdRunner, err := stdsql.NewRunner(new(sql.DB), stdsql.ScopePolicy[scope]{}, stdsql.RunnerOptions{})
	require.NoError(t, err)

	reqCtx := context.WithValue(ctx, ctxKey{}, "pgx-req-7")

	// 1. Role, timeouts, guard, cross-module ErrNested, Savepoint child proof, CopyFrom checks, BeforeCommit.
	var retained *pgxdriver.Session[scope]
	var retainedChildSP *pgxdriver.Session[scope]
	require.NoError(t, runner.WithinWith(reqCtx, "t1", pgxdriver.RunOptions[scope]{
		BeforeCommit: func(bCtx context.Context, bs *pgxdriver.Session[scope]) error {
			_, closedErr := retained.Exec(bCtx, "SELECT 1")
			require.ErrorIs(t, closedErr, tabula.ErrSessionClosed)
			_, cnt, err := tbl.Bind(bs).List(bCtx, nil, tabula.Listing{})
			require.NoError(t, err)
			require.Equal(t, int64(2), cnt)
			return nil
		},
	}, func(txCtx context.Context, s *pgxdriver.Session[scope]) error {
		retained = s
		var curRole, appScope, appMode, appReq string
		require.NoError(t, s.Scan(txCtx, `SELECT current_user, current_setting('app.scope'), current_setting('app.mode'), current_setting('app.req_id')`,
			nil, &curRole, &appScope, &appMode, &appReq))
		require.Equal(t, roleName, curRole)
		require.Equal(t, "t1", appScope)
		require.Equal(t, "scoped", appMode)
		require.Equal(t, "pgx-req-7", appReq)

		// Built-in guard rejects SET ROLE / set_config / timeout overrides.
		_, gErr := s.Exec(txCtx, "SET ROLE postgres")
		require.ErrorIs(t, gErr, tabula.ErrForbiddenStatement)
		_, gErr = s.Exec(txCtx, "SELECT set_config('app.scope', 't2', true)")
		require.ErrorIs(t, gErr, tabula.ErrForbiddenStatement)

		// Cross-module ErrNested: stdsql runner must reject pgxdriver's marked context before BEGIN.
		require.ErrorIs(t, stdRunner.Within(txCtx, "t1", func(context.Context, *stdsql.Session[scope]) error {
			return nil
		}), tabula.ErrNested)

		// Savepoint child receives fresh proof (A4) and is closed after Savepoint returns.
		require.NoError(t, s.Savepoint(txCtx, func(spCtx context.Context, child *pgxdriver.Session[scope]) error {
			retainedChildSP = child
			row := item{Tenant: "t1", Label: "sp-child", SecretToken: "hidden-secret"}
			require.NoError(t, tbl.Bind(child).Insert(spCtx, &row))
			for got, err := range tbl.Bind(child).Iter(spCtx, nil, tabula.Listing{}) {
				require.NoError(t, err)
				requireSessionBusy(t, func() error {
					_, err := tbl.Bind(child).Get(spCtx, got.ID)
					return err
				})
				requireSessionBusy(t, func() error {
					return s.Savepoint(spCtx, func(context.Context, *pgxdriver.Session[scope]) error { return nil })
				})
				requireSessionBusy(t, func() error {
					_, err := pgxdriver.CopyFrom(spCtx, s, tbl, []item{row})
					return err
				})
				break
			}
			return nil
		}))
		_, childClosedErr := retainedChildSP.Exec(txCtx, "SELECT 1")
		require.ErrorIs(t, childClosedErr, tabula.ErrSessionClosed)

		// CopyFrom checks DenyCopy BEFORE empty-input shortcut (A6).
		_, err := pgxdriver.CopyFrom(txCtx, s, denyTbl, nil)
		require.ErrorIs(t, err, tabula.ErrCapabilityDenied)

		// CopyFrom checks ErrForeignSession BEFORE empty-input shortcut (A4).
		unsealedTx, err := pool.Begin(tabula.WithoutTransaction(txCtx))
		require.NoError(t, err)
		t.Cleanup(func() { _ = unsealedTx.Rollback(context.Background()) })
		unsealedSess := pgxdriver.NewSession(unsealedTx, scope("t1"), policy)
		_, err = pgxdriver.CopyFrom(txCtx, unsealedSess, tbl, nil)
		require.ErrorIs(t, err, tabula.ErrForeignSession)

		// CopyFrom succeeds with sealed session.
		n, err := pgxdriver.CopyFrom(txCtx, s, tbl, []item{{Tenant: "t1", Label: "copied"}})
		require.NoError(t, err)
		require.Equal(t, int64(1), n)
		return nil
	}))
	_, err = retained.Exec(ctx, "SELECT 1")
	require.ErrorIs(t, err, tabula.ErrSessionClosed)
	// Empty CopyFrom on closed valid session returns ErrSessionClosed.
	_, err = pgxdriver.CopyFrom(ctx, retained, tbl, nil)
	require.ErrorIs(t, err, tabula.ErrSessionClosed)

	// B1: Runner owned Tracer so Registry didn't double-trace; secrets redacted.
	require.Empty(t, regTrace.snapshot())
	require.NotEmpty(t, runnerTrace.snapshot())
	require.NotContains(t, runnerLogBuf.String(), "hidden-secret")
	require.Contains(t, runnerLogBuf.String(), "[redacted]")

	// 2. CopyFrom enforces DenyUnscopedWrites before empty-input shortcut.
	require.NoError(t, runner.Within(reqCtx, "all", func(txCtx context.Context, s *pgxdriver.Session[scope]) error {
		_, err := pgxdriver.CopyFrom(txCtx, s, unscopedDenyTbl, nil)
		require.ErrorIs(t, err, tabula.ErrCapabilityDenied)
		return nil
	}))

	// 3. A3 WithinShared: scoped -> unscoped -> scoped + savepoint rollback + same handle + child invalidation.
	var sharedChild *pgxdriver.Session[scope]
	require.NoError(t, runner.WithinShared(reqCtx, func(txCtx context.Context, sh *pgxdriver.Shared[scope]) error {
		s1 := sh.Scoped("t1")
		all := sh.Unscoped()
		s2 := sh.Scoped("t2")
		sharedChild = s1

		checkVars := func(s *pgxdriver.Session[scope], wantScope, wantMode string) {
			var gotScope, gotMode string
			require.NoError(t, s.Scan(txCtx, "SELECT current_setting('app.scope'), current_setting('app.mode')", nil, &gotScope, &gotMode))
			require.Equal(t, wantScope, gotScope)
			require.Equal(t, wantMode, gotMode)
		}
		checkVars(s1, "t1", "scoped")
		checkVars(all, "", "unscoped")
		checkVars(s2, "t2", "scoped")

		for got, err := range tbl.Bind(s1).Iter(txCtx, nil, tabula.Listing{}) {
			require.NoError(t, err)
			requireSessionBusy(t, func() error {
				_, err := tbl.Bind(sh.Unscoped()).Get(txCtx, got.ID)
				return err
			})
			requireSessionBusy(t, func() error {
				return all.Savepoint(txCtx, func(context.Context, *pgxdriver.Session[scope]) error { return nil })
			})
			break
		}
		checkVars(s1, "t1", "scoped")

		// Nested savepoint rollback inside WithinShared followed by reusing s1.
		spErr := s2.Savepoint(txCtx, func(spCtx context.Context, child *pgxdriver.Session[scope]) error {
			checkVars(child, "t2", "scoped")
			return errors.New("rollback pgx sp")
		})
		require.ErrorContains(t, spErr, "rollback pgx sp")
		checkVars(s1, "t1", "scoped")
		return nil
	}))
	_, err = sharedChild.Exec(ctx, "SELECT 1")
	require.ErrorIs(t, err, tabula.ErrSessionClosed)

	// 4. A5 ExposeTx: WithinRaw.
	rawRunner, err := pgxdriver.NewRunner(pool, policy, pgxdriver.RunnerOptions{
		Role:       roleName,
		SessionKey: key,
		ExposeTx:   true,
	})
	require.NoError(t, err)
	require.NoError(t, rawRunner.WithinRaw(reqCtx, "t1", func(txCtx context.Context, rs *pgxdriver.RawSession[scope]) error {
		require.NotNil(t, rs.Tx())
		_, c1, err := tbl.Bind(rs).List(txCtx, nil, tabula.Listing{})
		require.NoError(t, err)
		_, c2, err := tbl.Bind(rs.Session()).List(txCtx, nil, tabula.Listing{})
		require.NoError(t, err)
		require.Equal(t, c1, c2)
		return nil
	}))

	// 5. A4 Keyed DirectWithConfig preserves native pgxpool.Config options + read-only + signing.
	keyedDirect, err := pgxdriver.DirectWithConfig(ctx, dsn, pgxdriver.ScopePolicy[scope]{}, pgxdriver.DirectConfig{
		SessionKey: key,
	}, func(cfg *pgxpool.Config) {
		cfg.MaxConns = 3
		cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "off"
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = keyedDirect.Close() })
	require.Equal(t, int32(3), keyedDirect.Stat().MaxConns())
	_, c, err := tbl.Bind(keyedDirect.Session("t1")).List(ctx, nil, tabula.Listing{})
	require.NoError(t, err)
	require.Equal(t, int64(2), c)
	var roOn bool
	require.NoError(t, keyedDirect.Session("t1").Scan(ctx, "SELECT current_setting('transaction_read_only')::bool", nil, &roOn))
	require.True(t, roOn)

	// Unkeyed Direct is rejected by sealed registry.
	unkeyedDirect, err := pgxdriver.Direct(ctx, dsn, pgxdriver.ScopePolicy[scope]{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = unkeyedDirect.Close() })
	_, _, err = tbl.Bind(unkeyedDirect.Session("t1")).List(ctx, nil, tabula.Listing{})
	require.ErrorIs(t, err, tabula.ErrForeignSession)

	// 6. A7 WithinRetry retries and invalidates prior handle.
	var retryAttempts int
	var prevSess *pgxdriver.Session[scope]
	transient := errors.New("pgx transient")
	require.NoError(t, runner.WithinRetry(reqCtx, "t1", tabula.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		ShouldRetry: func(err error) bool { return errors.Is(err, transient) },
	}, func(txCtx context.Context, s *pgxdriver.Session[scope]) error {
		retryAttempts++
		if retryAttempts == 1 {
			prevSess = s
			return transient
		}
		_, closedErr := prevSess.Exec(txCtx, "SELECT 1")
		require.ErrorIs(t, closedErr, tabula.ErrSessionClosed)
		return nil
	}))
	require.Equal(t, 2, retryAttempts)

	// Verify trace captured CopyFrom and inline SQL.
	var sawCopy, sawInline bool
	for _, ev := range runnerTrace.snapshot() {
		if ev.info.Action == "copy" {
			sawCopy = true
		}
		if strings.HasPrefix(ev.info.Key, "tabula.inline.") {
			sawInline = true
		}
	}
	require.True(t, sawCopy)
	require.True(t, sawInline)
}
