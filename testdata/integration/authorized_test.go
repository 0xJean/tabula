//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/stdsql"
	"github.com/stretchr/testify/require"
)

type authorityItem struct {
	ID      int64     `db:"id,key,serial"`
	Scope   itemScope `db:"scope_id,scope"`
	Label   string    `db:"label,secret"`
	Removed bool      `db:"removed,deleted"`
}

type authorityTrace struct{ keys []string }

func (tr *authorityTrace) Start(ctx context.Context, info tabula.TraceInfo) (context.Context, func(int64, error)) {
	tr.keys = append(tr.keys, info.Key)
	return ctx, func(int64, error) {}
}

func TestAuthorizedDescriptorSessions(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx := t.Context()
			ns := fmt.Sprintf("tabula_authority_%d", time.Now().UnixNano())
			table := ns + ".items"
			_, err := db.ExecContext(ctx, "CREATE SCHEMA "+ns)
			require.NoError(t, err)
			t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + ns + " CASCADE"); require.NoError(t, err) })
			_, err = db.ExecContext(ctx, "CREATE TABLE "+table+" (id bigserial PRIMARY KEY, scope_id bigint NOT NULL, label text NOT NULL, removed boolean NOT NULL, UNIQUE(scope_id,label))")
			require.NoError(t, err)
			seal := tabula.NewSessionKey()
			reg := tabula.NewRegistry(tabula.RegistryConfig[itemScope]{Deny: tabula.DenyLegacySQL, SessionKey: seal})
			spec := tabula.TableSpec[authorityItem, int64]{Name: table, Order: "id DESC", AllowDelete: true, GroupBy: []string{"scope_id"}, Aggregates: tabula.ScalarAggregates{Max: []string{"id"}}}
			tab := tabula.Define(reg, spec)
			foreign := tabula.Define(tabula.NewRegistry(tabula.RegistryConfig[itemScope]{SessionKey: seal}), spec)
			for _, empty := range []tabula.Session[itemScope]{new(stdsql.RawSession[itemScope]), new(pgxdriver.RawSession[itemScope])} {
				_, err := tab.Bind(empty).Get(ctx, 1)
				require.ErrorIs(t, err, tabula.ErrForeignSession)
			}
			unregistered, err := tabula.TryDefine(reg, spec)
			require.NoError(t, err)
			planSpec := tabula.UpsertSpec{Conflict: []string{"scope_id", "label"}, Returning: []string{"id"}}
			plan := tab.DefineUpsert("add_item", planSpec)
			otherPlan, err := tab.TryDefineUpsert("add_item", planSpec)
			require.NoError(t, err)
			auth := reg.SQLAuthority()
			trace := new(authorityTrace)
			policy := tabula.StatementPolicy{Read: []string{"select"}, Write: []string{"insert", "update", "delete"}, RequireReturning: true}
			var key int64
			var saved tabula.Session[itemScope]
			var savedCtx context.Context
			run := func(ctx context.Context, s tabula.Session[itemScope]) error {
				saved = s
				savedCtx = ctx
				rows := tab.Bind(s)
				row := authorityItem{Scope: 1, Label: "first"}
				require.NoError(t, rows.Insert(ctx, &row))
				require.Positive(t, row.ID)
				key = row.ID
				before := len(trace.keys)
				query := tab.Definition().Statements["get"]
				marked := tabula.WithStatementMeta(ctx, tabula.StatementMeta{Key: table + ".get"})
				require.ErrorIs(t, s.Scan(marked, query, []any{key, 1}), tabula.ErrLegacySQL)
				_, err := s.Exec(marked, tab.Definition().Statements["insert"])
				require.ErrorIs(t, err, tabula.ErrLegacySQL)
				require.ErrorIs(t, s.Each(marked, query, nil, nil), tabula.ErrLegacySQL)
				require.ErrorIs(t, s.Returning(marked, query, nil), tabula.ErrLegacySQL)
				require.ErrorIs(t, rows.Scan(ctx, query, []any{key, 1}), tabula.ErrLegacySQL)
				require.Equal(t, before, len(trace.keys))
				_, err = foreign.Bind(s).Get(ctx, key)
				require.ErrorIs(t, err, tabula.ErrStatementAuthority)
				_, err = unregistered.Bind(s).Get(ctx, key)
				require.ErrorIs(t, err, tabula.ErrStatementAuthority)
				_, err = rows.Get(t.Context(), key)
				require.ErrorIs(t, err, tabula.ErrCallbackContext)
				_, _, err = rows.List(ctx, func(*tabula.Filter) { t.Fatal("unexpected filter call") }, tabula.Listing{})
				require.ErrorIs(t, err, tabula.ErrLegacySQL)
				_, _, err = rows.List(ctx, nil, tabula.Listing{Size: 1})
				require.ErrorIs(t, err, tabula.ErrLegacySQL)
				_, err = rows.Upsert(ctx, otherPlan, &row)
				require.ErrorIs(t, err, tabula.ErrLegacySQL)
				_, err = rows.UpsertMany(ctx, plan, []authorityItem{row})
				require.ErrorIs(t, err, tabula.ErrLegacySQL)
				require.ErrorIs(t, rows.Set(ctx, row, "label"), tabula.ErrLegacySQL)
				got, err := rows.Lock(ctx, key)
				require.NoError(t, err)
				require.Equal(t, &row, got)
				row.Label = "changed"
				require.NoError(t, rows.Save(ctx, row))
				another := authorityItem{Label: "second", Scope: 1}
				inserted, err := rows.Upsert(ctx, plan, &another)
				require.NoError(t, err)
				require.True(t, inserted)
				list, total, err := rows.List(ctx, nil, tabula.Listing{})
				require.NoError(t, err)
				require.EqualValues(t, 2, total)
				require.Equal(t, []authorityItem{another, row}, list)
				counts, err := tabula.CountBy[int64](ctx, rows, "scope_id", nil)
				require.NoError(t, err)
				require.Equal(t, map[int64]int64{1: 2}, counts)
				maxID, ok, err := tabula.Max[int64](ctx, rows, "id", nil)
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, another.ID, maxID)
				_, _, err = tabula.Min[int64](ctx, rows, "id", nil)
				require.ErrorIs(t, err, tabula.ErrLegacySQL)
				require.NoError(t, rows.Delete(ctx, another.ID))
				return nil
			}
			observe := func(c context.Context, s tabula.Session[itemScope], want bool) error {
				got, err := tab.Bind(s).Get(c, key)
				require.NoError(t, err)
				require.Equal(t, want, got != nil)
				return nil
			}
			isUnscoped := func(scope itemScope) bool { return scope == 0 }
			dsn := os.Getenv("TABULA_TEST_DSN")
			var direct tabula.Session[itemScope]
			switch driver {
			case "stdsql":
				scope := stdsql.ScopePolicy[itemScope]{IsUnscoped: isUnscoped}
				r, err := stdsql.NewRunner(db, scope, stdsql.RunnerOptions{SQLAuthority: auth, SessionKey: seal, RequireCallbackContext: true, Unscoped: stdsql.UnscopedReadWrite, ExposeTx: true, Statements: policy, Tracer: trace})
				require.NoError(t, err)
				require.ErrorIs(t, r.WithinRaw(ctx, 1, func(context.Context, *stdsql.RawSession[itemScope]) error {
					t.Fatal("raw callback entered")
					return nil
				}), tabula.ErrLegacySQL)
				require.NoError(t, r.Within(ctx, 1, func(c context.Context, s *stdsql.Session[itemScope]) error { return run(c, s) }))
				require.NoError(t, r.WithinShared(ctx, func(c context.Context, sh *stdsql.Shared[itemScope]) error {
					s := sh.Scoped(2)
					require.NoError(t, observe(c, s, false))
					return s.Savepoint(c, "read_items", func(c context.Context) error { return observe(c, sh.Unscoped(), true) })
				}))
				p, err := stdsql.Direct("postgres", dsn, scope, stdsql.DirectSessionKey(seal), stdsql.DirectSQLAuthority(auth))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, p.Close()) })
				direct = p.Session(1)
			case "pgx":
				scope := pgxdriver.ScopePolicy[itemScope]{IsUnscoped: isUnscoped}
				r, err := pgxdriver.NewRunner(pool, scope, pgxdriver.RunnerOptions{SQLAuthority: auth, SessionKey: seal, RequireCallbackContext: true, Unscoped: pgxdriver.UnscopedReadWrite, ExposeTx: true, Statements: policy, Tracer: trace})
				require.NoError(t, err)
				require.ErrorIs(t, r.WithinRaw(ctx, 1, func(context.Context, *pgxdriver.RawSession[itemScope]) error {
					t.Fatal("raw callback entered")
					return nil
				}), tabula.ErrLegacySQL)
				require.NoError(t, r.Within(ctx, 1, func(c context.Context, s *pgxdriver.Session[itemScope]) error {
					_, err := pgxdriver.CopyFrom(c, s, foreign, nil)
					require.ErrorIs(t, err, tabula.ErrStatementAuthority)
					return run(c, s)
				}))
				require.NoError(t, r.WithinShared(ctx, func(c context.Context, sh *pgxdriver.Shared[itemScope]) error {
					require.NoError(t, observe(c, sh.Scoped(2), false))
					return sh.Unscoped().Savepoint(c, func(c context.Context, s *pgxdriver.Session[itemScope]) error { return observe(c, s, true) })
				}))
				p, err := pgxdriver.DirectWithConfig(ctx, dsn, scope, pgxdriver.DirectConfig{SessionKey: seal, SQLAuthority: auth})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, p.Close()) })
				direct = p.Session(1)
			}
			_, err = tab.Bind(saved).Get(savedCtx, key)
			require.ErrorIs(t, err, tabula.ErrSessionClosed)
			require.NoError(t, observe(ctx, direct, true))
			_, err = tab.Bind(direct).Lock(ctx, key)
			require.ErrorIs(t, err, tabula.ErrReadOnly)
			visible, err := tab.Bind(direct).Find(ctx, nil, tabula.Listing{})
			require.NoError(t, err)
			require.Len(t, visible, 1)
			require.ErrorIs(t, direct.Scan(ctx, tab.Definition().Statements["get"], []any{key, 1}), tabula.ErrLegacySQL)
			require.ErrorIs(t, direct.Each(ctx, "SELECT 1", nil, nil), tabula.ErrLegacySQL)
			n, err := direct.Exec(ctx, "SELECT 1")
			require.Zero(t, n)
			require.ErrorIs(t, err, tabula.ErrLegacySQL)
			require.ErrorIs(t, err, tabula.ErrReadOnly)
			for _, key := range []string{"insert", "lock", "update", "add_item", "get", "unscoped_get", "list", "count", "delete"} {
				require.Contains(t, trace.keys, table+"."+key)
			}
		})
	}
}
