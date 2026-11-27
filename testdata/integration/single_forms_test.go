//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

type singleItem struct {
	ID    int64   `db:"id,key"`
	Group int32   `db:"group_id"`
	Note  *string `db:"note"`
	State string  `db:"state,deleted"`
}

type singleResult struct {
	ID   int64   `db:"id"`
	Note *string `db:"note"`
}

func prepareSingleTables(t *testing.T, db *sql.DB) (string, string) {
	t.Helper()
	schema := fmt.Sprintf("single_rows_%d", time.Now().UnixNano())
	role := schema + "_reader"
	exec := func(query string) {
		t.Helper()
		_, err := db.ExecContext(t.Context(), query)
		require.NoError(t, err)
	}
	exec("CREATE ROLE " + role)
	t.Cleanup(func() { _, err := db.Exec("DROP ROLE " + role); require.NoError(t, err) })
	exec("CREATE SCHEMA " + schema)
	t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + schema + " CASCADE"); require.NoError(t, err) })
	exec("GRANT USAGE ON SCHEMA " + schema + " TO " + role)
	items := schema + ".items"
	exec("CREATE TABLE " + items + " (id bigint PRIMARY KEY, group_id int NOT NULL, note text, state text NOT NULL)")
	exec("INSERT INTO " + items + " VALUES (1,1,NULL,'open'),(2,2,'two','open'),(3,2,'three','open')," +
		"(4,3,NULL,'open'),(5,3,NULL,'open'),(6,3,NULL,'open'),(7,4,NULL,'closed'),(8,4,NULL,'open')")
	exec("ALTER TABLE " + items + " ENABLE ROW LEVEL SECURITY")
	exec("ALTER TABLE " + items + " FORCE ROW LEVEL SECURITY")
	exec("CREATE POLICY visible ON " + items + " TO " + role + " USING (id <> 8)")
	exec("GRANT SELECT(id,group_id,note,state), UPDATE(note) ON " + items + " TO " + role)
	return schema, role
}

type singleCalls struct {
	one, first           func(plan.Binding) (singleResult, error)
	maybeOne, maybeFirst func(plan.Binding) (singleResult, bool, error)
}

func checkSingleResults(t *testing.T, calls singleCalls, key plan.Slot[int64]) {
	t.Helper()
	for _, limit := range []int64{0, 1, 2, 6} {
		arg := plan.BindValue(key, limit)
		row, err := calls.one(arg)
		maybe, found, optErr := calls.maybeOne(arg)
		switch limit {
		case 0:
			require.ErrorIs(t, err, q.ErrNotFound)
			require.ErrorIs(t, err, sql.ErrNoRows)
			require.NoError(t, optErr)
			require.False(t, found)
			require.Zero(t, row)
			require.Zero(t, maybe)
		case 1:
			require.NoError(t, err)
			require.NoError(t, optErr)
			require.True(t, found)
			require.Equal(t, singleResult{ID: 1}, row)
			require.Equal(t, row, maybe)
		default:
			require.ErrorIs(t, err, q.ErrMultipleRows)
			require.ErrorIs(t, optErr, q.ErrMultipleRows)
			require.False(t, found)
			require.Zero(t, row)
			require.Zero(t, maybe)
		}
		first, err := calls.first(arg)
		maybe, found, optErr = calls.maybeFirst(arg)
		require.NoError(t, optErr)
		require.Equal(t, limit > 0, found)
		if limit == 0 {
			require.ErrorIs(t, err, q.ErrNotFound)
			require.ErrorIs(t, err, sql.ErrNoRows)
			require.Zero(t, first)
		} else {
			require.NoError(t, err)
			require.Equal(t, singleResult{ID: 1}, first)
		}
		require.Equal(t, first, maybe)
	}
}

func TestSingleReadDrivers(t *testing.T) {
	db, pool := databases(t)
	schema, role := prepareSingleTables(t, db)
	reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
	table := q.DefinePlanSource(reg, q.TableSpec[singleItem, int64]{Name: schema + ".items", RelationScope: q.ByRLS(),
		SoftDelete: &q.SoftDelete{AliveValue: "open", DeletedValue: "closed"}})
	left, right := plan.FromTable(table, "a", q.ByRLS()), plan.FromTable(table, "b", q.ByRLS())
	src := plan.RowsFrom(table, left)
	id, otherID := plan.FieldOf(left, "id", plan.Int8()), plan.FieldOf(right, "id", plan.Int8())
	group, peer := plan.FieldOf(left, "group_id", plan.Int4()), plan.FieldOf(right, "group_id", plan.Int4())
	key := plan.Input("key", plan.Int8())
	filter := plan.RowFilter{Where: plan.Le(id.Expr(), key.Expr())}
	out := q.DefineView(reg, q.ViewSpec[singleResult]{Name: "entries"})
	project := []plan.Projection{plan.Project(plan.Output(out, "id", plan.Int8()), id.Expr()),
		plan.Project(plan.Output(out, "note", plan.Text()), plan.FieldOf(left, "note", plan.Text()).Expr())}
	forms := plan.ReadForms{Exact: true, First: plan.OrderedFirst}
	sort := plan.RowOrder{Name: "forward", Terms: []plan.RowTerm{plan.Asc(id)}}
	spec := plan.RowsSpec{RowFilter: filter, Orders: []plan.RowOrder{sort}, WithExists: true}
	rows := plan.DefineRowsWithForms(out, "by_key", src, spec, forms, project...)
	probe := plan.DefineExists(src, "visible", plan.RowFilter{Optional: []plan.RowPredicate{{Name: "by_id", When: plan.Eq(id.Expr(), key.Expr())}}})
	match := plan.Select(project...).From(src).InnerJoin(right, plan.Eq(group.Expr(), peer.Expr())).
		Where(filter.Where, plan.Le(otherID.Expr(), key.Expr()))
	joined := plan.DefineRead(out, "paired", match.OrderBy(plan.Asc(id), plan.Asc(otherID)).WithForms(forms))
	unordered := plan.DefineRead(out, "any_pair", match.WithForms(plan.ReadForms{First: plan.UnorderedFirst}))
	totals := q.DefineView(reg, q.ViewSpec[lockResult]{Name: "notes"})
	count := plan.Project(plan.Output(totals, "id", plan.Int8()), plan.CountAll())
	scalar := plan.DefineRows(totals, "total", src, plan.RowsSpec{RowFilter: filter}, count)
	grouped := plan.DefineRead(totals, "totals", plan.Select(count).From(src).Where(filter.Where).
		GroupBy(group.Expr()).OrderBy(plan.Asc(group)).WithForms(forms))

	spec.Lock, spec.WithExists = plan.ForRows(plan.ForUpdate, left, plan.SkipLocked), false
	locked := plan.DefineRowsWithForms(out, "locked", src, spec, forms, project...)
	joinedLock := plan.DefineRead(out, "locked_pairs", match.OrderBy(plan.Asc(id), plan.Asc(otherID)).
		Lock(plan.ForRows(plan.ForUpdate, left, plan.SkipLocked)).WithForms(forms))
	work := sqlcheck.RoleStatements{}
	work.AddDefinitions(role, table.Definition())
	work.AddViews(role, out.Definition(), totals.Definition())
	needs, err := work.Readiness(role)
	require.NoError(t, err)
	missing, err := sqlcheck.CheckReadiness(t.Context(), db, role, needs)
	require.NoError(t, err)
	require.Empty(t, missing)
	findings, err := sqlcheck.VerifyRoleStatements(t.Context(), db, work)
	require.NoError(t, err)
	require.Empty(t, findings)

	choice := plan.RowChoice{Order: "forward"}
	ctx := t.Context()
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			auth := reg.SQLAuthority()
			run := authorizedEffectRunner(t, driver, db, pool, role, auth, q.StatementPolicy{}, nil, false)
			require.NoError(t, run(ctx, func(c context.Context, s session) error {
				b := rows.Bind(s)
				checkSingleResults(t, singleCalls{
					one:        func(v plan.Binding) (singleResult, error) { return b.One(c, choice, v) },
					first:      func(v plan.Binding) (singleResult, error) { return b.First(c, choice, v) },
					maybeOne:   func(v plan.Binding) (singleResult, bool, error) { return b.MaybeOne(c, choice, v) },
					maybeFirst: func(v plan.Binding) (singleResult, bool, error) { return b.MaybeFirst(c, choice, v) },
				}, key)
				j := joined.Bind(s)
				checkSingleResults(t, singleCalls{
					one:        func(v plan.Binding) (singleResult, error) { return j.One(c, v) },
					first:      func(v plan.Binding) (singleResult, error) { return j.First(c, v) },
					maybeOne:   func(v plan.Binding) (singleResult, bool, error) { return j.MaybeOne(c, v) },
					maybeFirst: func(v plan.Binding) (singleResult, bool, error) { return j.MaybeFirst(c, v) },
				}, key)
				pairs, err := j.Find(c, plan.BindValue(key, 3))
				require.NoError(t, err)
				require.Len(t, pairs, 5)
				require.Equal(t, pairs[1], pairs[2])
				first, err := unordered.Bind(s).First(c, plan.BindValue(key, 3))
				require.NoError(t, err)
				require.Contains(t, pairs, first)
				for _, id := range []int64{1, 7, 8, 9} {
					got, err := probe.Bind(s).Exists(c, []string{"by_id"}, plan.BindValue(key, id))
					require.NoError(t, err)
					require.Equal(t, id == 1, got)
				}
				for _, n := range []int64{0, 1} {
					got, err := b.Exists(c, nil, plan.BindValue(key, n))
					require.NoError(t, err)
					require.Equal(t, n != 0, got)
				}
				total, found, err := scalar.Bind(s).MaybeOne(c, plan.RowChoice{}, plan.BindValue(key, 0))
				require.NoError(t, err)
				require.True(t, found)
				require.Zero(t, total.ID)
				_, found, err = grouped.Bind(s).MaybeFirst(c, plan.BindValue(key, 0))
				require.NoError(t, err)
				require.False(t, found)
				total, err = grouped.Bind(s).One(c, plan.BindValue(key, 1))
				require.NoError(t, err)
				require.EqualValues(t, 1, total.ID)
				_, err = grouped.Bind(s).One(c, plan.BindValue(key, 6))
				require.ErrorIs(t, err, q.ErrMultipleRows)
				// The completed cursor releases the session for the next read.
				found, err = probe.Bind(s).Exists(c, nil)
				require.True(t, found)
				return err
			}))
			all := plan.BindValue(key, 10)
			for _, hold := range []int{1, 2} {
				require.NoError(t, run(ctx, func(c context.Context, s session) error {
					b := locked.Bind(s)
					if hold == 1 {
						first, found, err := b.MaybeFirst(c, choice, all)
						require.NoError(t, err)
						require.True(t, found)
						require.EqualValues(t, 1, first.ID)
					} else {
						_, err := b.One(c, choice, all)
						require.ErrorIs(t, err, q.ErrMultipleRows)
					}
					return run(ctx, func(other context.Context, s session) error {
						free, err := locked.Bind(s).Find(other, choice, all)
						require.NoError(t, err)
						require.Len(t, free, 6-hold)
						require.EqualValues(t, hold+1, free[0].ID)
						return nil
					})
				}))
			}
			ro := authorizedEffectRunner(t, driver, db, pool, role, auth, q.StatementPolicy{}, nil, true)
			checkReadOnly := func(c context.Context, s session) error {
				_, _, err := joinedLock.Bind(s).MaybeOne(c, all)
				require.ErrorIs(t, err, q.ErrReadOnly)
				_, err = locked.Bind(s).First(c, choice, all)
				return err
			}
			require.ErrorIs(t, ro(ctx, checkReadOnly), q.ErrReadOnly)
			require.ErrorIs(t, checkReadOnly(ctx, effectDirect(t, driver, auth)), q.ErrReadOnly)
			err := run(ctx, func(c context.Context, s session) error {
				cancelled, cancel := context.WithCancel(c)
				cancel()
				row, found, err := joined.Bind(s).MaybeFirst(cancelled, all)
				require.Zero(t, row)
				require.False(t, found)
				return err
			})
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestSingleReadLateError(t *testing.T) {
	db, pool := databases(t)
	ns, role := prepareSingleTables(t, db)
	notes := ns + ".notes"
	// The server emits two rows before evaluating the failing expression.
	_, err := db.ExecContext(t.Context(), "CREATE VIEW "+notes+" AS "+
		"SELECT CASE WHEN n <= 2 THEN n ELSE 1 / (n - n) END AS id FROM generate_series(1::bigint,3) n")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "GRANT SELECT ON "+notes+" TO "+role)
	require.NoError(t, err)
	type record struct {
		ID int64 `db:"id,key"`
	}
	reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
	table := q.DefinePlanSource(reg, q.TableSpec[record, int64]{Name: notes, RelationScope: q.Global()})
	rel := plan.FromTable(table, "n", q.Global())
	src := plan.RowsFrom(table, rel)
	out := q.DefineView(reg, q.ViewSpec[lockResult]{Name: "entries"})
	project := plan.Project(plan.Output(out, "id", plan.Int8()), plan.FieldOf(rel, "id", plan.Int8()).Expr())
	unbounded := plan.DefineRows(out, "all_notes", src, plan.RowsSpec{}, project)
	bounded := plan.DefineRowsWithForms(out, "some_notes", src, plan.RowsSpec{}, plan.ReadForms{Exact: true}, project)
	// A mismatched result type fails in each driver's result decoder.
	entries := ns + ".entries"
	_, err = db.ExecContext(t.Context(), "CREATE VIEW "+entries+" AS SELECT 'invalid'::text AS id")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "GRANT SELECT ON "+entries+" TO "+role)
	require.NoError(t, err)
	bad := q.DefinePlanSource(reg, q.TableSpec[record, int64]{Name: entries, RelationScope: q.Global()})
	b := plan.FromTable(bad, "e", q.Global())
	scan := plan.DefineRead(out, "entry", plan.Select(plan.Project(plan.Output(out, "id", plan.Int8()), plan.FieldOf(b, "id", plan.Int8()).Expr())).
		From(b).WithForms(plan.ReadForms{First: plan.UnorderedFirst}))
	available := plan.DefineExists(plan.RowsFrom(bad, b), "present", plan.RowFilter{})
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			run := authorizedEffectRunner(t, driver, db, pool, role, reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			read := func(ctx context.Context, s session) error {
				row, found, err := unbounded.Bind(s).MaybeOne(ctx, plan.RowChoice{})
				require.Zero(t, row)
				require.False(t, found)
				return err
			}
			err := run(t.Context(), read)
			require.Equal(t, "22012", q.SQLState(err), "%v", err)
			direct := effectDirect(t, driver, reg.SQLAuthority())
			err = read(t.Context(), direct)
			require.Equal(t, "22012", q.SQLState(err), "%v", err)
			// The bounded query completes after its second row on the server.
			_, err = bounded.Bind(direct).One(t.Context(), plan.RowChoice{})
			require.ErrorIs(t, err, q.ErrMultipleRows)
			require.NoError(t, run(t.Context(), func(c context.Context, s session) error {
				row, found, err := scan.Bind(s).MaybeFirst(c)
				require.Error(t, err)
				require.NotErrorIs(t, err, q.ErrNotFound)
				require.Zero(t, row)
				require.False(t, found)
				found, err = available.Bind(s).Exists(c, nil)
				require.True(t, found)
				return err
			}))
		})
	}
}
