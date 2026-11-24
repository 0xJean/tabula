//go:build integration

package stdsql_test

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

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/stdsql"
)

type traceRec struct {
	mu     sync.Mutex
	events []traceEvent
}

type traceEvent struct {
	info tabula.TraceInfo
	rows int64
	err  error
}

func (r *traceRec) Start(ctx context.Context, info tabula.TraceInfo) (context.Context, func(int64, error)) {
	return ctx, func(rows int64, err error) {
		r.mu.Lock()
		r.events = append(r.events, traceEvent{info: info, rows: rows, err: err})
		r.mu.Unlock()
	}
}

func (r *traceRec) snapshot() []traceEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]traceEvent, len(r.events))
	copy(out, r.events)
	return out
}

type metricRec struct {
	mu     sync.Mutex
	events []metricEvent
}

type metricEvent struct {
	info  tabula.TraceInfo
	rows  int64
	class tabula.ErrorClass
}

func (m *metricRec) Observe(_ context.Context, info tabula.TraceInfo, _ time.Duration, rows int64, errClass tabula.ErrorClass) {
	m.mu.Lock()
	m.events = append(m.events, metricEvent{info: info, rows: rows, class: errClass})
	m.mu.Unlock()
}

func (m *metricRec) snapshot() []metricEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]metricEvent, len(m.events))
	copy(out, m.events)
	return out
}

func TestV031RoleRunnerSharedSealedAndObserve(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	ctx := t.Context()

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	schema := "tabula_v031_std_" + suffix
	roleName := "tabula_role_" + suffix

	_, err = db.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })

	_, err = db.ExecContext(ctx, `CREATE ROLE `+roleName+` NOLOGIN`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DROP ROLE IF EXISTS ` + roleName) })

	_, err = db.ExecContext(ctx, `CREATE TABLE `+schema+`.items (
		id bigserial PRIMARY KEY,
		tenant_id text NOT NULL,
		label text NOT NULL,
		secret_token text NOT NULL DEFAULT ''
	)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `GRANT USAGE ON SCHEMA `+schema+` TO `+roleName)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA `+schema+` TO `+roleName)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA `+schema+` TO `+roleName)
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
	regTrace := &traceRec{}
	regMetrics := &metricRec{}
	var regLogBuf bytes.Buffer
	reg := tabula.NewRegistry(tabula.RegistryConfig[scope]{
		RequireSealed: true,
		ScopeID:       func(s scope) any { return string(s) },
		ScopeLog:      func(s scope) string { return "reg:" + string(s) },
		Tracer:        regTrace,
		Metrics:       regMetrics,
		Log: tabula.LogConfig{
			Logger: slog.New(slog.NewTextHandler(&regLogBuf, nil)),
			Level:  tabula.LogAll,
			Values: tabula.ValuesAllowed,
		},
	})
	tbl := tabula.Define(reg, tabula.TableSpec[item, int64]{Name: schema + ".items", Order: "id ASC"})
	require.NoError(t, reg.Seal(key))

	runnerTrace := &traceRec{}
	var runnerLogBuf bytes.Buffer
	policy := stdsql.ScopePolicy[scope]{
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
	runner, err := stdsql.NewRunner(db, policy, stdsql.RunnerOptions{
		Role:          roleName,
		RequireRole:   true,
		Unscoped:      stdsql.UnscopedReadWrite,
		SessionKey:    key,
		Tracer:        runnerTrace,
		TagStatements: true,
		Log: tabula.LogConfig{
			Logger: slog.New(slog.NewTextHandler(&runnerLogBuf, nil)),
			Level:  tabula.LogAll,
			Values: tabula.ValuesAllowed,
			SQL:    true,
		},
		Timeouts: stdsql.Timeouts{
			Lock:              2 * time.Second,
			Statement:         5 * time.Second,
			IdleInTransaction: 10 * time.Second,
			Transaction:       15 * time.Second,
		},
	})
	require.NoError(t, err)

	reqCtx := context.WithValue(ctx, ctxKey{}, "req-42")

	// 1. Role, timeouts, context GUCs, guard, nesting, BeforeCommit, session close, B1 precedence & redaction.
	var retained *stdsql.Session[scope]
	var beforeCommitRan bool
	require.NoError(t, runner.WithinWith(reqCtx, "t1", stdsql.RunOptions[scope]{
		BeforeCommit: func(bCtx context.Context, bs *stdsql.Session[scope]) error {
			beforeCommitRan = true
			// Body handle must already be closed before BeforeCommit runs.
			_, closedErr := retained.Exec(bCtx, "SELECT 1")
			require.ErrorIs(t, closedErr, tabula.ErrSessionClosed)
			// Fresh handle is signed and works with sealed registry.
			_, count, err := tbl.Bind(bs).List(bCtx, nil, tabula.Listing{})
			require.NoError(t, err)
			require.Equal(t, int64(1), count)
			return nil
		},
	}, func(txCtx context.Context, s *stdsql.Session[scope]) error {
		retained = s
		// Verify role, timeouts, and GUCs.
		var curRole, lockMS, stmtMS, idleMS, appScope, appMode, appReq string
		require.NoError(t, s.Scan(txCtx, `SELECT current_user, current_setting('lock_timeout'), current_setting('statement_timeout'), current_setting('idle_in_transaction_session_timeout'), current_setting('app.scope'), current_setting('app.mode'), current_setting('app.req_id')`,
			nil, &curRole, &lockMS, &stmtMS, &idleMS, &appScope, &appMode, &appReq))
		require.Equal(t, roleName, curRole)
		require.Equal(t, "2s", lockMS)
		require.Equal(t, "5s", stmtMS)
		require.Equal(t, "10s", idleMS)
		require.Equal(t, "t1", appScope)
		require.Equal(t, "scoped", appMode)
		require.Equal(t, "req-42", appReq)

		// Built-in guard blocks SET ROLE, set_config, and timeout overrides.
		for _, bad := range []string{
			"SET ROLE postgres",
			"SET SESSION AUTHORIZATION DEFAULT",
			"SELECT set_config('app.scope', 't2', true)",
			"SET lock_timeout = '100s'",
			"SET statement_timeout = 0",
		} {
			_, gErr := s.Exec(txCtx, bad)
			require.ErrorIs(t, gErr, tabula.ErrForbiddenStatement, bad)
		}

		// Nested runner call returns ErrNested before BEGIN.
		require.ErrorIs(t, runner.Within(txCtx, "t2", func(context.Context, *stdsql.Session[scope]) error {
			return nil
		}), tabula.ErrNested)

		// Descriptor insert with secret column + inline query.
		row := item{Tenant: "t1", Label: "alpha", SecretToken: "top-secret-value"}
		require.NoError(t, tbl.Bind(s).Insert(txCtx, &row))
		require.NotZero(t, row.ID)

		// Unsealed NewSession must be rejected with ErrForeignSession.
		unsealed := stdsql.NewSession(nilSafeTx(t, db), scope("t1"), policy)
		_, fErr := tbl.Bind(unsealed).Get(txCtx, row.ID)
		require.ErrorIs(t, fErr, tabula.ErrForeignSession)

		return nil
	}))
	require.True(t, beforeCommitRan)
	// Retained handle fails closed after callback.
	_, err = retained.Exec(ctx, "SELECT 1")
	require.ErrorIs(t, err, tabula.ErrSessionClosed)

	// B1: Runner owned Tracer & Log; Registry owned Metrics (one-owner-per-signal).
	require.Empty(t, regTrace.snapshot(), "runner owns Tracer so registry must not double-trace")
	require.Empty(t, regLogBuf.String(), "runner owns Log so registry must not double-log")
	require.NotEmpty(t, regMetrics.snapshot(), "runner had no Metrics so registry still emits metrics")
	rEvents := runnerTrace.snapshot()
	require.GreaterOrEqual(t, len(rEvents), 3)
	var sawInline, sawInsert bool
	for _, ev := range rEvents {
		require.Equal(t, roleName, ev.info.Role)
		if strings.HasPrefix(ev.info.Key, "tabula.inline.") {
			sawInline = true
		}
		if ev.info.Key == schema+".items.insert" {
			sawInsert = true
		}
	}
	require.True(t, sawInline)
	require.True(t, sawInsert)
	logOut := runnerLogBuf.String()
	require.NotContains(t, logOut, "top-secret-value", "secret column must be redacted in runner logs")
	require.Contains(t, logOut, "[redacted]")
	require.Contains(t, logOut, "tabula.role="+roleName)

	// 2. BeforeCommit error rolls back body writes.
	err = runner.WithinWith(reqCtx, "t1", stdsql.RunOptions[scope]{
		BeforeCommit: func(context.Context, *stdsql.Session[scope]) error {
			return errors.New("abort before commit")
		},
	}, func(txCtx context.Context, s *stdsql.Session[scope]) error {
		row := item{Tenant: "t1", Label: "rolled-back", SecretToken: "x"}
		return tbl.Bind(s).Insert(txCtx, &row)
	})
	require.ErrorContains(t, err, "abort before commit")
	require.NoError(t, runner.WithinReadOnly(reqCtx, "t1", func(txCtx context.Context, s *stdsql.Session[scope]) error {
		_, n, err := tbl.Bind(s).List(txCtx, nil, tabula.Listing{})
		require.NoError(t, err)
		require.Equal(t, int64(1), n)
		return nil
	}))

	// 3. A3 WithinShared: scoped -> unscoped -> scoped, savepoint rollback + same handle, child invalidation.
	var retainedChild *stdsql.Session[scope]
	require.NoError(t, runner.WithinShared(reqCtx, func(txCtx context.Context, sh *stdsql.Shared[scope]) error {
		s1 := sh.Scoped("t1")
		all := sh.Unscoped()
		s2 := sh.Scoped("t2")
		retainedChild = s1

		checkVars := func(s *stdsql.Session[scope], wantScope, wantMode string) {
			var gotScope, gotMode string
			require.NoError(t, s.Scan(txCtx, "SELECT current_setting('app.scope'), current_setting('app.mode')", nil, &gotScope, &gotMode))
			require.Equal(t, wantScope, gotScope)
			require.Equal(t, wantMode, gotMode)
		}
		checkVars(s1, "t1", "scoped")
		checkVars(all, "", "unscoped")
		checkVars(s2, "t2", "scoped")

		// Savepoint rollback restores prior PG GUC state, then reusing s1 must reapply s1's state.
		spErr := s2.Savepoint(txCtx, "sp_switch", func(spCtx context.Context) error {
			checkVars(all, "", "unscoped")
			return errors.New("rollback sp")
		})
		require.ErrorContains(t, spErr, "rollback sp")
		checkVars(s1, "t1", "scoped")

		// Insert via s2 works with sealed registry.
		row2 := item{Tenant: "t2", Label: "beta"}
		require.NoError(t, tbl.Bind(s2).Insert(txCtx, &row2))

		// Early break on Iter via unscoped handle cleanly closes cursor and leaves shared tx usable.
		var seen int
		for _, iterErr := range tbl.Bind(all).Iter(txCtx, nil, tabula.Listing{}) {
			require.NoError(t, iterErr)
			seen++
			break
		}
		require.Equal(t, 1, seen)
		checkVars(s2, "t2", "scoped")
		return nil
	}))
	_, err = retainedChild.Exec(ctx, "SELECT 1")
	require.ErrorIs(t, err, tabula.ErrSessionClosed)

	// 4. A5 ExposeTx: raw runner vs default runner.
	rawRunner, err := stdsql.NewRunner(db, policy, stdsql.RunnerOptions{
		Role:       roleName,
		SessionKey: key,
		ExposeTx:   true,
	})
	require.NoError(t, err)
	require.NoError(t, rawRunner.WithinRaw(reqCtx, "t1", func(txCtx context.Context, rs *stdsql.RawSession[scope]) error {
		require.NotNil(t, rs.Tx())
		// Both *RawSession and rs.Session() carry valid proofs for sealed registry.
		_, n1, err := tbl.Bind(rs).List(txCtx, nil, tabula.Listing{})
		require.NoError(t, err)
		_, n2, err := tbl.Bind(rs.Session()).List(txCtx, nil, tabula.Listing{})
		require.NoError(t, err)
		require.Equal(t, n1, n2)
		return nil
	}))

	// 5. A4 Keyed DirectPool works with sealed registry.
	directPool, err := stdsql.Direct("postgres", dsn, stdsql.ScopePolicy[scope]{}, stdsql.DirectSessionKey(key))
	require.NoError(t, err)
	t.Cleanup(func() { _ = directPool.Close() })
	_, n, err := tbl.Bind(directPool.Session("t1")).List(ctx, nil, tabula.Listing{})
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	// Unkeyed DirectPool is rejected by sealed registry.
	unkeyedDirect, err := stdsql.Direct("postgres", dsn, stdsql.ScopePolicy[scope]{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = unkeyedDirect.Close() })
	_, _, err = tbl.Bind(unkeyedDirect.Session("t1")).List(ctx, nil, tabula.Listing{})
	require.ErrorIs(t, err, tabula.ErrForeignSession)

	// 6. A7 WithinRetry retries retryable error and runs fresh session per attempt.
	attempts := 0
	var firstAttemptSess *stdsql.Session[scope]
	require.NoError(t, runner.WithinRetry(reqCtx, "t1", tabula.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		ShouldRetry: func(err error) bool { return errors.Is(err, errFakeTransient) },
	}, func(txCtx context.Context, s *stdsql.Session[scope]) error {
		attempts++
		if attempts == 1 {
			firstAttemptSess = s
			return errFakeTransient
		}
		_, closedErr := firstAttemptSess.Exec(txCtx, "SELECT 1")
		require.ErrorIs(t, closedErr, tabula.ErrSessionClosed)
		return nil
	}))
	require.Equal(t, 2, attempts)

	// 7. Panic inside callback closes session and rolls back.
	var panicSess *stdsql.Session[scope]
	require.Panics(t, func() {
		_ = runner.Within(reqCtx, "t1", func(txCtx context.Context, s *stdsql.Session[scope]) error {
			panicSess = s
			row := item{Tenant: "t1", Label: "panic-row"}
			require.NoError(t, tbl.Bind(s).Insert(txCtx, &row))
			panic("boom")
		})
	})
	_, err = panicSess.Exec(ctx, "SELECT 1")
	require.ErrorIs(t, err, tabula.ErrSessionClosed)
	require.NoError(t, runner.WithinReadOnly(reqCtx, "t1", func(txCtx context.Context, s *stdsql.Session[scope]) error {
		_, cnt, err := tbl.Bind(s).List(txCtx, nil, tabula.Listing{})
		require.NoError(t, err)
		require.Equal(t, int64(1), cnt)
		return nil
	}))

	// 8. Early Iter break must not record an error in runnerTracer.
	beforeCount := len(runnerTrace.snapshot())
	require.NoError(t, runner.WithinReadOnly(reqCtx, "all", func(txCtx context.Context, s *stdsql.Session[scope]) error {
		for _, err := range tbl.Bind(s).Iter(txCtx, nil, tabula.Listing{}) {
			require.NoError(t, err)
			break
		}
		return nil
	}))
	afterEvents := runnerTrace.snapshot()[beforeCount:]
	require.Len(t, afterEvents, 1)
	require.NoError(t, afterEvents[0].err, "early cursor break must not be recorded as a trace error")
	require.Equal(t, int64(1), afterEvents[0].rows)
}

var errFakeTransient = errors.New("fake transient")

func nilSafeTx(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}
