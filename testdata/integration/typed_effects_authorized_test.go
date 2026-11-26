//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/0xJean/tabula/stdsql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

type effectRunner = func(context.Context, callback) error

func authorizedEffectRunner(t *testing.T, driver string, db *sql.DB, pool *pgxpool.Pool, role string, auth q.SQLAuthority, policy q.StatementPolicy, guard func(string) error, readOnly bool) effectRunner {
	t.Helper()
	if driver == "pq" {
		r, err := stdsql.NewRunner(db, stdsql.ScopePolicy[struct{}]{GuardQuery: guard}, stdsql.RunnerOptions{
			Role: role, SQLAuthority: auth, RequireCallbackContext: true, Statements: policy,
		})
		require.NoError(t, err)
		run := r.Within
		if readOnly {
			run = r.WithinReadOnly
		}
		return func(ctx context.Context, fn callback) error {
			return run(ctx, struct{}{}, adapt[*stdsql.Session[struct{}]](fn))
		}
	}
	r, err := pgxdriver.NewRunner(pool, pgxdriver.ScopePolicy[struct{}]{GuardQuery: guard}, pgxdriver.RunnerOptions{
		Role: role, SQLAuthority: auth, RequireCallbackContext: true, Statements: policy,
	})
	require.NoError(t, err)
	run := r.Within
	if readOnly {
		run = r.WithinReadOnly
	}
	return func(ctx context.Context, fn callback) error {
		return run(ctx, struct{}{}, adapt[*pgxdriver.Session[struct{}]](fn))
	}
}

func effectDirect(t *testing.T, driver string, auth q.SQLAuthority) session {
	t.Helper()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	if driver == "pq" {
		p, err := stdsql.Direct("postgres", dsn, stdsql.NoScopePolicy(), stdsql.DirectSQLAuthority(auth))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, p.Close()) })
		return p.Session(struct{}{})
	}
	p, err := pgxdriver.DirectWithConfig(t.Context(), dsn, pgxdriver.NoScopePolicy(), pgxdriver.DirectConfig{SQLAuthority: auth})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, p.Close()) })
	return p.Session(struct{}{})
}

func TestTypedLockRoutineAuthorized(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			f := newEffectFixture(t, db, reg)
			auth := reg.SQLAuthority()
			run := authorizedEffectRunner(t, driver, db, pool, f.role, auth, q.StatementPolicy{}, nil, false)
			ro := authorizedEffectRunner(t, driver, db, pool, f.role, auth, q.StatementPolicy{}, nil, true)
			id := q.BindValue(f.id, int64(1))
			require.NoError(t, run(ctx, func(c context.Context, s session) error {
				first, err := f.routines["small"].Bind(s).One(c, q.BindValue(f.small, int32(9)))
				require.NoError(t, err)
				require.EqualValues(t, 109, first.ID)
				second, err := f.routines["large"].Bind(s).One(c, q.BindValue(f.id, int64(9)))
				require.NoError(t, err)
				require.EqualValues(t, 209, second.ID)
				_, err = f.routines["large"].Bind(s).One(ctx, q.BindValue(f.id, int64(9)))
				require.ErrorIs(t, err, q.ErrCallbackContext)
				for _, n := range []int64{0, 1, 3} {
					rows, err := f.routines["series"].Bind(s).All(c, q.BindValue(f.id, n))
					require.NoError(t, err)
					require.Len(t, rows, int(n))
					for i, row := range rows {
						require.EqualValues(t, i+1, row.ID)
					}
				}
				inserted, err := f.routines["add_one"].Bind(s).One(c, q.BindValue(f.id, int64(7)))
				require.NoError(t, err)
				require.EqualValues(t, 7, inserted.ID)
				many, err := f.routines["add_set"].Bind(s).All(c, q.BindValue(f.id, int64(2)))
				require.NoError(t, err)
				require.Equal(t, []lockResult{{1}, {2}}, many)
				for _, lock := range f.statusLocks {
					visible, err := lock.Bind(s).Find(c, q.RowChoice{})
					require.NoError(t, err)
					require.Equal(t, []lockResult{{1}}, visible)
				}
				held, err := f.row[q.Wait].Bind(s).Find(c, q.RowChoice{}, id)
				require.NoError(t, err)
				require.Equal(t, []lockResult{{1}}, held)
				require.NoError(t, run(ctx, func(other context.Context, s session) error {
					got, err := f.row[q.SkipLocked].Bind(s).Find(other, q.RowChoice{}, id)
					require.Empty(t, got)
					return err
				}))
				err = run(ctx, func(other context.Context, s session) error {
					_, err := f.row[q.NoWait].Bind(s).Find(other, q.RowChoice{}, id)
					return err
				})
				require.Equal(t, "55P03", q.SQLState(err))
				return nil
			}))
			checkTransactionLocks(ctx, t, run, func(ctx context.Context, s session, name string, key int64, pair [2]int32) (bool, error) {
				if name == "attempt" || name == "big" {
					return f.advisory[name].Bind(s).Acquire(ctx, q.BindValue(f.id, key))
				}
				return f.advisory[name].Bind(s).Acquire(ctx, q.BindValue(f.small, pair[0]), q.BindValue(f.part, pair[1]))
			})
			checkRestricted := func(ctx context.Context, s session) {
				t.Helper()
				_, err := f.row[q.Wait].Bind(s).Find(ctx, q.RowChoice{}, id)
				require.ErrorIs(t, err, q.ErrReadOnly)
				require.Empty(t, q.SQLState(err))
				_, err = f.routines["add_one"].Bind(s).One(ctx, id)
				require.ErrorIs(t, err, q.ErrReadOnly)
				require.Empty(t, q.SQLState(err))
				_, err = f.routines["add_set"].Bind(s).All(ctx, id)
				require.ErrorIs(t, err, q.ErrReadOnly)
				require.Empty(t, q.SQLState(err))
				row, err := f.routines["large"].Bind(s).One(ctx, id)
				require.NoError(t, err)
				require.EqualValues(t, 201, row.ID)
			}
			require.NoError(t, ro(ctx, func(c context.Context, s session) error {
				checkRestricted(c, s)
				held, err := f.advisory["attempt"].Bind(s).Acquire(c, id)
				require.True(t, held)
				return err
			}))
			direct := effectDirect(t, driver, auth)
			checkRestricted(ctx, direct)
			for _, name := range []string{"big", "attempt"} {
				_, err := f.advisory[name].Bind(direct).Acquire(ctx, id)
				require.ErrorIs(t, err, q.ErrReadOnly)
				require.Empty(t, q.SQLState(err))
			}

			// The write channel checks Returning's outer-command policy for both cardinalities.
			policy := q.StatementPolicy{Read: []string{"select"}, Write: []string{"insert", "update"}, RequireReturning: true}
			restricted := authorizedEffectRunner(t, driver, db, pool, f.role, auth, policy, nil, false)
			require.NoError(t, restricted(ctx, func(c context.Context, s session) error {
				_, err := f.routines["large"].Bind(s).One(c, id)
				require.NoError(t, err)
				_, err = f.routines["add_one"].Bind(s).One(c, id)
				require.ErrorIs(t, err, q.ErrForbiddenStatement)
				_, err = f.routines["add_set"].Bind(s).All(c, id)
				require.ErrorIs(t, err, q.ErrForbiddenStatement)
				return nil
			}))
			var count int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+f.schema+".entries").Scan(&count))
			require.Equal(t, 3, count)

			setting := q.Input("setting", q.Text())
			texts := q.DefineView(reg, q.ViewSpec[struct {
				Value string `db:"value"`
			}]{Name: "groups"})
			current := q.DeclareRoutine(q.RoutineSpec[string]{Schema: "pg_catalog", Name: "current_setting", Result: q.Text(), Cardinality: q.RoutineOne, ReadOnly: true}, q.Parameter("setting", q.Text()))
			get := q.DefineRoutine(texts, "setting", current, q.Arg(q.FormalOf(current, "setting", q.Text()), setting.Expr()))
			guard, err := q.NewSessionGuard(q.SessionGuardConfig{VariablePrefixes: []string{"demo"}, ForbidVariableReads: true})
			require.NoError(t, err)
			guarded := authorizedEffectRunner(t, driver, db, pool, f.role, auth, q.StatementPolicy{}, guard, false)
			err = guarded(ctx, func(c context.Context, s session) error {
				_, err := get.Bind(s).One(c, q.BindValue(setting, "demo.scope"))
				return err
			})
			require.ErrorIs(t, err, q.ErrForbiddenStatement)

			// A changed catalog declaration fails readiness. PostgreSQL's actual
			// read-only transaction also preserves its error and SQLSTATE.
			drift := q.DeclareRoutine(q.RoutineSpec[int64]{Schema: f.schema, Name: "add_one", Result: q.Int8(), Cardinality: q.RoutineOne, ReadOnly: true, SecurityDefiner: true}, q.Parameter("id", q.Int8()))
			drifted := q.DefineRoutine(f.view, "drift", drift, q.Arg(q.FormalOf(drift, "id", q.Int8()), f.id.Expr()))
			missing, err := sqlcheck.CheckReadiness(ctx, db, f.role, effectReadiness(t, f.view.Definition(), "drift.call"))
			require.NoError(t, err)
			require.Len(t, missing, 1)
			err = ro(ctx, func(c context.Context, s session) error {
				_, err := drifted.Bind(s).One(c, id)
				return err
			})
			require.ErrorIs(t, err, q.ErrReadOnly)
			require.Equal(t, "25006", q.SQLState(err))
			_, err = db.ExecContext(ctx, "REVOKE EXECUTE ON FUNCTION "+f.schema+".lookup(bigint) FROM "+f.role)
			require.NoError(t, err)
			err = run(ctx, func(c context.Context, s session) error {
				_, err := f.routines["large"].Bind(s).One(c, id)
				return err
			})
			require.Equal(t, "42501", q.SQLState(err))
			_, err = db.ExecContext(ctx, "REVOKE UPDATE(note) ON "+f.table+" FROM "+f.role)
			require.NoError(t, err)
			err = run(ctx, func(c context.Context, s session) error {
				_, err := f.row[q.Wait].Bind(s).Find(c, q.RowChoice{}, id)
				return err
			})
			require.Equal(t, "42501", q.SQLState(err))
		})
	}
}
