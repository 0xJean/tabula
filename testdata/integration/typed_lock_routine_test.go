//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/0xJean/tabula/stdsql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

type lockItem struct {
	ID      int64 `db:"id,key"`
	Removed bool  `db:"removed,deleted"`
}

type lockResult struct {
	ID int64 `db:"id"`
}
type advisoryResult struct {
	Held bool `db:"held"`
}

type effectFixture struct {
	reg                 *q.Registry[struct{}]
	view                *q.View[lockResult, struct{}]
	flags               *q.View[advisoryResult, struct{}]
	role, schema, table string

	id          q.Slot[int64]
	small, part q.Slot[int32]
	row         map[q.LockWait]*q.RowsPlan[lockResult, struct{}]
	statusLocks map[string]*q.RowsPlan[lockResult, struct{}]
	advisory    map[string]*q.AdvisoryLockPlan[advisoryResult, struct{}]
	routines    map[string]*q.RoutinePlan[lockResult, struct{}]
}

func newEffectFixture(t *testing.T, db *sql.DB, reg *q.Registry[struct{}]) *effectFixture {
	t.Helper()
	ns := fmt.Sprintf("tabula_lock_%d", time.Now().UnixNano())
	f := &effectFixture{schema: ns, role: ns + "_reader", table: ns + ".items", reg: reg}
	exec := func(sql string) {
		t.Helper()
		_, err := db.ExecContext(t.Context(), sql)
		require.NoError(t, err)
	}
	exec("CREATE ROLE " + f.role)
	t.Cleanup(func() {
		_, err := db.Exec("DROP ROLE " + f.role)
		require.NoError(t, err)
	})
	exec("CREATE SCHEMA " + ns)
	t.Cleanup(func() {
		_, err := db.Exec("DROP SCHEMA " + ns + " CASCADE")
		require.NoError(t, err)
	})
	for _, sql := range []string{
		"CREATE TABLE " + f.table + " (id bigint PRIMARY KEY, removed boolean NOT NULL, note text)",
		"INSERT INTO " + f.table + " VALUES (1, false), (2, true)",
		"CREATE TABLE " + ns + ".entries (id bigint)",
		"CREATE FUNCTION " + ns + ".lookup(integer) RETURNS bigint LANGUAGE SQL IMMUTABLE AS 'SELECT $1::bigint + 100'",
		"CREATE FUNCTION " + ns + ".lookup(bigint) RETURNS bigint LANGUAGE SQL STABLE AS 'SELECT $1 + 200'",
		"CREATE FUNCTION " + ns + ".series(bigint) RETURNS SETOF bigint LANGUAGE SQL STABLE AS 'SELECT generate_series(1::bigint, $1)'",
		"CREATE FUNCTION " + ns + ".add_one(bigint) RETURNS bigint LANGUAGE SQL SECURITY DEFINER SET search_path=pg_catalog AS 'INSERT INTO " + ns + ".entries VALUES ($1) RETURNING id'",
		"CREATE FUNCTION " + ns + ".add_set(bigint) RETURNS SETOF bigint LANGUAGE SQL SECURITY DEFINER SET search_path=pg_catalog AS 'INSERT INTO " + ns + ".entries SELECT generate_series(1::bigint, $1) RETURNING id'",
		"REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA " + ns + " FROM PUBLIC",
		"GRANT USAGE ON SCHEMA " + ns + " TO " + f.role,
		"GRANT SELECT(id, removed), UPDATE(note) ON " + f.table + " TO " + f.role,
		"GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA " + ns + " TO " + f.role,
	} {
		exec(sql)
	}
	f.view = q.DefineView(f.reg, q.ViewSpec[lockResult]{Name: "entries"})
	f.flags = q.DefineView(f.reg, q.ViewSpec[advisoryResult]{Name: "notes"})
	tab := q.Define(f.reg, q.TableSpec[lockItem, int64]{Name: f.table, RelationScope: q.Global()})
	rel := q.FromTable(tab, "i", q.Global())
	id := q.FieldOf(rel, "id", q.Int8()).Expr()
	f.id, f.small, f.part = q.Input("key", q.Int8()), q.Input("first", q.Int4()), q.Input("second", q.Int4())
	f.row = make(map[q.LockWait]*q.RowsPlan[lockResult, struct{}])
	for wait, key := range map[q.LockWait]string{q.Wait: "hold", q.NoWait: "nowait", q.SkipLocked: "skip"} {
		f.row[wait] = q.DefineRows(f.view, key, q.RowsFrom(tab, rel), q.RowsSpec{
			Lock: q.ForRows(q.ForUpdate, rel, wait), RowFilter: q.RowFilter{Where: q.Eq(id, f.id.Expr())},
		}, q.Project(q.Output(f.view, "id", q.Int8()), id))
	}
	f.advisory = make(map[string]*q.AdvisoryLockPlan[advisoryResult, struct{}])
	for key, lock := range map[string]q.AdvisoryLock{
		"big":       q.AdvisoryXactLock(q.Exclusive, q.Wait, f.id.Expr()),
		"attempt":   q.AdvisoryXactLock(q.Exclusive, q.Try, f.id.Expr()),
		"pair":      q.AdvisoryXactLockPair(q.Shared, q.Wait, f.small.Expr(), f.part.Expr()),
		"share":     q.AdvisoryXactLockPair(q.Shared, q.Try, f.small.Expr(), f.part.Expr()),
		"exclusive": q.AdvisoryXactLockPair(q.Exclusive, q.Try, f.small.Expr(), f.part.Expr()),
	} {
		f.advisory[key] = q.DefineAdvisoryLock(f.flags, key, lock)
	}
	f.routines = make(map[string]*q.RoutinePlan[lockResult, struct{}])
	spec := q.RoutineSpec[int64]{Schema: ns, Name: "lookup", Result: q.Int8(), Cardinality: q.RoutineOne, ReadOnly: true}
	small := q.DeclareRoutine(spec, q.Parameter("id", q.Int4()))
	f.routines["small"] = q.DefineRoutine(f.view, "small", small, q.Arg(q.FormalOf(small, "id", q.Int4()), f.small.Expr()))
	call := func(key string, spec q.RoutineSpec[int64]) {
		fn := q.DeclareRoutine(spec, q.Parameter("id", q.Int8()))
		f.routines[key] = q.DefineRoutine(f.view, key, fn, q.Arg(q.FormalOf(fn, "id", q.Int8()), f.id.Expr()))
	}
	call("large", spec)
	spec.Name, spec.Cardinality = "series", q.RoutineSet
	call("series", spec)
	spec.Name, spec.Cardinality, spec.ReadOnly, spec.SecurityDefiner = "add_one", q.RoutineOne, false, true
	call("add_one", spec)
	spec.Name, spec.Cardinality = "add_set", q.RoutineSet
	call("add_set", spec)
	f.statusLocks = make(map[string]*q.RowsPlan[lockResult, struct{}])
	exec("CREATE TYPE " + ns + ".item_state AS ENUM ('open', 'closed')")
	exec("CREATE DOMAIN " + ns + ".item_rank AS integer")
	addStatusLock(t, db, f, "notes", "text", q.Text(), `open'\entry`, "closed", true)
	addStatusLock(t, db, f, "groups", ns+".item_state", q.CatalogType[string](ns, "item_state", nil), "open", "closed", true)
	addStatusLock(t, db, f, "jobs", ns+".item_rank", q.CatalogType[int32](ns, "item_rank", nil), int32(2), int32(7), false)
	return f
}

func effectReadiness(t *testing.T, def q.ViewDefinition, key string) sqlcheck.ReadinessNeeds {
	t.Helper()
	s, ok := def.StatementShapes[key]
	require.True(t, ok)
	grants, empty := sqlcheck.CollectPrivilegeNeeds([]q.Definition{{Name: def.Name, Privileges: s.Privileges}})
	require.Empty(t, empty)
	return sqlcheck.ReadinessNeeds{Privileges: grants, SchemaUsage: s.SchemaUsage, AnyColumnSelect: s.AnyColumnSelect, AnyColumnUpdate: s.AnyColumnUpdate, Routines: s.Routines}
}

// Verify the published SQL and minimum grants with each adapter under the
// restricted role, independently of the opaque-call dispatch tests.
func TestTypedLockRoutineSQL(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			f := newEffectFixture(t, db, q.NewRegistry(q.RegistryConfig[struct{}]{}))
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			run := effectSQLRunner(t, driver, db, pool, f.role)
			view := f.view.Definition()
			check := func(key string) []sqlcheck.MissingRequirement {
				t.Helper()
				missing, err := sqlcheck.CheckReadiness(ctx, db, f.role, effectReadiness(t, view, key))
				require.NoError(t, err)
				return missing
			}
			for key := range view.Statements {
				require.Empty(t, check(key))
			}
			failures, err := sqlcheck.VerifyViewStatements(ctx, db, f.reg.DefinedViews(), f.role)
			require.NoError(t, err)
			require.Empty(t, failures)
			advisory := f.flags.Definition()
			checkTransactionLocks(ctx, t, run, func(ctx context.Context, s session, key string, big int64, pair [2]int32) (bool, error) {
				args := []any{big}
				if key == "pair" || key == "share" || key == "exclusive" {
					args = []any{pair[0], pair[1]}
				}
				var held bool
				err := s.Scan(ctx, advisory.Statements[key+".lock"], args, &held)
				return held, err
			})
			rows := func(s session, sql string, value any) ([]int64, error) {
				var ids []int64
				err := s.Each(ctx, sql, []any{value}, func(scan func(...any) error) error {
					var id int64
					if err := scan(&id); err != nil {
						return err
					}
					ids = append(ids, id)
					return nil
				})
				return ids, err
			}
			for key, value := range map[string]any{"notes": `open'\entry`, "groups": "open", "jobs": int32(7)} {
				require.NoError(t, run(ctx, func(_ context.Context, s session) error {
					ids, err := rows(s, view.Statements[key+".global.data"], value)
					require.Equal(t, []int64{1}, ids)
					return err
				}))
			}
			// Keep the holder's transaction open until both competing reads return.
			require.NoError(t, run(ctx, func(_ context.Context, holder session) error {
				ids, err := rows(holder, view.Statements["hold.global.data"], 1)
				require.NoError(t, err)
				require.Equal(t, []int64{1}, ids)
				ids, err = rows(holder, view.Statements["hold.global.data"], 2)
				require.NoError(t, err)
				require.Empty(t, ids)
				require.NoError(t, run(ctx, func(_ context.Context, s session) error {
					ids, err := rows(s, view.Statements["skip.global.data"], 1)
					require.Empty(t, ids)
					return err
				}))
				err = run(ctx, func(_ context.Context, s session) error {
					_, err := rows(s, view.Statements["nowait.global.data"], 1)
					return err
				})
				require.Equal(t, "55P03", q.SQLState(err))
				return nil
			}))
			require.NoError(t, run(ctx, func(_ context.Context, s session) error {
				var small, big int64
				require.NoError(t, s.Scan(ctx, view.Statements["small.call"], []any{int32(7)}, &small))
				require.NoError(t, s.Scan(ctx, view.Statements["large.call"], []any{int64(7)}, &big))
				require.EqualValues(t, 107, small)
				require.EqualValues(t, 207, big)
				return nil
			}))
			_, err = db.ExecContext(ctx, "REVOKE EXECUTE ON FUNCTION "+f.schema+".lookup(bigint) FROM "+f.role)
			require.NoError(t, err)
			require.Empty(t, check("small.call"))
			missing := check("large.call")
			require.Len(t, missing, 1)
			require.Equal(t, "EXECUTE", missing[0].Privilege)
			require.Equal(t, view.StatementShapes["large.call"].Routines[0].Signature, missing[0].Object)
			err = run(ctx, func(ctx context.Context, s session) error {
				return s.Scan(ctx, view.Statements["large.call"], []any{int64(7)}, new(int64))
			})
			require.Equal(t, "42501", q.SQLState(err))
			_, err = db.ExecContext(ctx, "ALTER TABLE "+f.table+" ENABLE ROW LEVEL SECURITY")
			require.NoError(t, err)
			_, err = db.ExecContext(ctx, "CREATE POLICY read_items ON "+f.table+" FOR SELECT TO "+f.role+" USING (true)")
			require.NoError(t, err)
			probePolicy := func(expected []int64) {
				t.Helper()
				require.NoError(t, run(ctx, func(_ context.Context, s session) error {
					visible, err := rows(s, "SELECT id FROM "+f.table+" WHERE id = $1", 1)
					require.NoError(t, err)
					require.Equal(t, []int64{1}, visible)
					locked, err := rows(s, view.Statements["hold.global.data"], 1)
					require.Equal(t, expected, locked)
					return err
				}))
			}
			probePolicy(nil)
			_, err = db.ExecContext(ctx, "CREATE POLICY lock_items ON "+f.table+" FOR UPDATE TO "+f.role+" USING (true)")
			require.NoError(t, err)
			probePolicy([]int64{1})
			_, err = db.ExecContext(ctx, "REVOKE UPDATE(note) ON "+f.table+" FROM "+f.role)
			require.NoError(t, err)
			require.Equal(t, []sqlcheck.MissingRequirement{{Kind: "TABLE", Object: view.StatementShapes["hold.global.data"].AnyColumnUpdate[0], Privilege: "UPDATE ANY COLUMN"}}, check("hold.global.data"))
			err = run(ctx, func(_ context.Context, s session) error {
				_, err := rows(s, view.Statements["hold.global.data"], 1)
				return err
			})
			require.Equal(t, "42501", q.SQLState(err))
		})
	}
}

func checkTransactionLocks(ctx context.Context, t *testing.T, run func(context.Context, callback) error,
	acquire func(context.Context, session, string, int64, [2]int32) (bool, error),
) {
	t.Helper()
	// Use matching bits in PostgreSQL's two distinct advisory key spaces.
	key := time.Now().UnixNano()
	pair := [2]int32{int32(key >> 32), int32(key)}
	rollback := errors.New("roll back holder")
	for _, end := range []error{nil, rollback} {
		err := run(ctx, func(txCtx context.Context, s session) error {
			held, err := acquire(txCtx, s, "big", key, pair)
			require.NoError(t, err)
			require.True(t, held)
			held, err = acquire(txCtx, s, "pair", key, pair)
			require.NoError(t, err)
			require.True(t, held)
			require.NoError(t, run(ctx, func(otherCtx context.Context, other session) error {
				for name, want := range map[string]bool{"share": true, "exclusive": false, "attempt": false} {
					held, err := acquire(otherCtx, other, name, key, pair)
					require.NoError(t, err)
					require.Equal(t, want, held, name)
				}
				return nil
			}))
			return end
		})
		require.ErrorIs(t, err, end)
		require.NoError(t, run(ctx, func(txCtx context.Context, s session) error {
			for _, name := range []string{"exclusive", "attempt"} {
				held, err := acquire(txCtx, s, name, key, pair)
				require.NoError(t, err)
				require.True(t, held, name)
			}
			return nil
		}))
	}
}

func effectSQLRunner(t *testing.T, driver string, db *sql.DB, pool *pgxpool.Pool, role string) func(context.Context, callback) error {
	t.Helper()
	if driver == "pq" {
		r, err := stdsql.NewRunner(db, stdsql.NoScopePolicy(), stdsql.RunnerOptions{Role: role})
		require.NoError(t, err)
		return func(ctx context.Context, fn callback) error {
			return r.Within(ctx, struct{}{}, adapt[*stdsql.Session[struct{}]](fn))
		}
	}
	r, err := pgxdriver.NewRunner(pool, pgxdriver.NoScopePolicy(), pgxdriver.RunnerOptions{Role: role})
	require.NoError(t, err)
	return func(ctx context.Context, fn callback) error {
		return r.Within(ctx, struct{}{}, adapt[*pgxdriver.Session[struct{}]](fn))
	}
}
