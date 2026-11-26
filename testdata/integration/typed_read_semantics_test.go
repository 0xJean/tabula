//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

type listGroup struct {
	Label *string `db:"label"`
	Hits  int64   `db:"hits"`
}

func TestReadListResultSemanticsBothDrivers(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			f := newListFixture(t, db, driver)
			order := q.RowOrder{Name: "rank", Terms: []q.RowTerm{q.Desc(f.rank).NullsLast(), q.Asc(f.id)}}
			winners := q.DefineRead(f.out, "winners", f.query.DistinctOn(f.id.Expr()).OrderBy(q.Asc(f.id), q.Desc(f.rank).NullsLast(), q.Asc(f.nid)).WithForms(q.ReadForms{Exact: true}).
				WithList(q.ReadListSpec{Orders: []q.RowOrder{order}, Pages: q.RowPages{Unpaged: true, Keyset: true, MaxLimit: 3}, ExactCount: true}))
			nullable := q.DefineRead(f.out, "nullable_keys", f.query.DistinctOn(f.label.Expr()).OrderBy(q.Asc(f.label), q.Asc(f.id), q.Asc(f.nid)).WithList(q.ReadListSpec{ExactCount: true}))
			out := q.DefineView(f.reg, q.ViewSpec[listGroup]{Name: "groups"})
			hits := q.CountOf(f.nid.Expr())
			atLeast := q.Input("at_least", q.Int8())
			grouped := q.DefineRead(out, "grouped", q.Select(q.Project(q.Output(out, "label", q.Text()), f.label.Expr()), q.Project(q.Output(out, "hits", q.Int8()), hits)).
				From(f.items).LeftJoin(f.notes, q.Eq(f.parent.Expr(), f.id.Expr())).GroupBy(f.label.Expr()).Having(q.Ge(hits, atLeast.Expr())).
				WithList(q.ReadListSpec{Orders: []q.RowOrder{{Name: "label", Terms: []q.RowTerm{q.Asc(f.label).NullsFirst()}}}, Pages: q.RowPages{Unpaged: true, Keyset: true, MaxLimit: 2}, ExactCount: true}))
			type labelRow struct {
				Label *string `db:"label"`
			}
			labels := q.DefineView(f.reg, q.ViewSpec[labelRow]{Name: "labels"})
			distinct := q.DefineRead(labels, "distinct", q.Select(q.Project(q.Output(labels, "label", q.Text()), f.label.Expr())).From(f.items).Distinct().WithList(q.ReadListSpec{
				Orders: []q.RowOrder{{Name: "label", Terms: []q.RowTerm{q.Desc(f.label).NullsLast()}}}, Pages: q.RowPages{Unpaged: true, Keyset: true, MaxLimit: 1}, ExactCount: true}))
			type totalRow struct {
				Total int64 `db:"total"`
			}
			totals := q.DefineView(f.reg, q.ViewSpec[totalRow]{Name: "totals"})
			ids := q.Input("ids", q.ArrayOf(q.Int8()))
			aggregate := q.DefineRead(totals, "aggregate", q.Select(q.Project(q.Output(totals, "total", q.Int8()), q.CountAll())).From(f.items).Where(q.Any(f.id.Expr(), ids.Expr())).WithList(q.ReadListSpec{ExactCount: true}))

			orderOnly := q.DefineRead(totals, "order_aggregate", q.Select(q.Project(q.Output(totals, "total", q.Int8()), atLeast.Expr())).From(f.items).Where(q.Any(f.id.Expr(), ids.Expr())).OrderBy(q.AscExpr(q.CountAll())).WithList(q.ReadListSpec{Orders: []q.RowOrder{{Name: "constant", Terms: []q.RowTerm{q.AscExpr(atLeast.Expr())}}}, Pages: q.RowPages{Offset: true, MaxLimit: 1, MaxOffset: 2}, ExactCount: true}))
			verifyListSnapshot(t, db, f)
			// Winner proof is retained by fixed and bounded shapes and removed by the count.
			def := f.out.Definition()
			require.NotEmpty(t, def.StatementShapes["winners.scoped.data"].Keys)
			require.NotEmpty(t, def.StatementShapes["winners.scoped.single"].Keys)
			require.Empty(t, def.StatementShapes["winners.scoped.list.count"].Keys)
			require.NotContains(t, def.Statements["winners.scoped.list.count"], "ORDER BY")
			afterSQL := def.Statements["winners.scoped.list.data.sort.rank.after"]
			require.Greater(t, strings.Index(afterSQL, `AS "result" WHERE`), strings.Index(afterSQL, "DISTINCT ON"))
			err := runRowPlan(t, db, pool, driver, f.reg.SQLAuthority(), f.role, 1, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				expected := []listEntry{{1, new(int64(11)), new(int32(3))}, {2, new(int64(13)), new(int32(-2))}, {3, nil, nil}, {4, nil, nil}}
				all, total, err := winners.Bind(s).List(ctx, q.RowChoice{Order: "rank"})
				require.NoError(t, err)
				require.Equal(t, expected, all)
				require.EqualValues(t, 4, total)
				var combined []listEntry
				choice := q.RowChoice{Order: "rank", Page: q.FirstPage, Limit: 1}
				for range 6 {
					page, err := winners.Bind(s).Page(ctx, choice)
					require.NoError(t, err)
					if len(page) == 0 {
						break
					}
					combined = append(combined, page...)
					cursor, err := winners.CursorFrom("rank", page[0])
					require.NoError(t, err)
					choice.Page, choice.After = q.AfterPage, cursor
				}
				require.Equal(t, expected, combined) // no losing note becomes a second winner
				total, err = nullable.Bind(s).Count(ctx, nil)
				require.NoError(t, err)
				require.EqualValues(t, 3, total) // NULL is a group
				groups, n, err := grouped.Bind(s).List(ctx, q.RowChoice{Order: "label", Page: q.FirstPage, Limit: 1}, q.BindValue(atLeast, int64(0)))
				require.NoError(t, err)
				require.EqualValues(t, 3, n)
				require.Equal(t, []listGroup{{nil, 0}}, groups)
				cursor, err := grouped.CursorFrom("label", groups[0])
				require.NoError(t, err)
				groups, n, err = grouped.Bind(s).List(ctx, q.RowChoice{Order: "label", Page: q.AfterPage, Limit: 2, After: cursor}, q.BindValue(atLeast, int64(0)))
				require.NoError(t, err)
				require.EqualValues(t, 3, n)
				require.Equal(t, []listGroup{{new("a"), 2}, {new("b"), 2}}, groups)
				n, err = grouped.Bind(s).Count(ctx, nil, q.BindValue(atLeast, int64(3)))
				require.NoError(t, err)
				require.Zero(t, n)
				want := []labelRow{{new("b")}, {new("a")}, {nil}}
				var got []labelRow
				dc := q.RowChoice{Order: "label", Page: q.FirstPage, Limit: 1}
				for range 5 {
					page, n, err := distinct.Bind(s).List(ctx, dc)
					require.NoError(t, err)
					require.EqualValues(t, 3, n)
					if len(page) == 0 {
						break
					}
					got = append(got, page...)
					cursor, err := distinct.CursorFrom("label", page[0])
					require.NoError(t, err)
					dc.Page, dc.After = q.AfterPage, cursor
				}
				require.Equal(t, want, got)
				scalar, n, err := aggregate.Bind(s).List(ctx, q.RowChoice{}, q.BindValue(ids, []int64{}))
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				require.Equal(t, []totalRow{{0}}, scalar)
				scalar, n, err = orderOnly.Bind(s).List(ctx, q.RowChoice{Order: "constant", Page: q.OffsetPage, Limit: 1}, q.BindValue(ids, []int64{}), q.BindValue(atLeast, int64(7)))
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				require.Equal(t, []totalRow{{7}}, scalar)

				return nil
			})
			require.NoError(t, err)
		})
	}
}

func TestReadListMinimumSummaryGrantsBothDrivers(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			f := newListFixture(t, db, driver)
			out := q.DefineView(f.reg, q.ViewSpec[listGroup]{Name: "groups"})
			boundSlot := q.Input("projected", q.Int8())
			plan := q.DefineRead(out, "minimal", q.Select(q.Project(q.Output(out, "label", q.Text()), f.label.Expr()), q.Project(q.Output(out, "hits", q.Int8()), boundSlot.Expr())).From(f.items).WithList(q.ReadListSpec{ExactCount: true}))
			def := out.Definition()
			key := "minimal.scoped.list.count"
			shape := def.StatementShapes[key]
			require.Len(t, shape.Inputs, 1)
			require.NotContains(t, shape.Privileges[0].Columns, "label")
			_, err := db.ExecContext(t.Context(), "REVOKE SELECT(label) ON "+f.schema+".items FROM "+f.role)
			require.NoError(t, err)
			selected := q.Definition{Name: def.Name, Statements: map[string]string{key: def.Statements[key]}, StatementQueries: map[string]string{key: "minimal"}, StatementShapes: map[string]q.StatementShape{key: shape}}
			work := sqlcheck.RoleStatements{}
			work.AddDefinitions(f.role, selected)
			needs, err := work.Readiness(f.role)
			require.NoError(t, err)
			missing, err := sqlcheck.CheckReadiness(t.Context(), db, f.role, needs)
			require.NoError(t, err)
			require.Empty(t, missing)
			failures, err := sqlcheck.VerifyRoleStatements(t.Context(), db, work)
			require.NoError(t, err)
			require.Empty(t, failures)
			err = runRowPlan(t, db, pool, driver, f.reg.SQLAuthority(), f.role, 1, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				n, err := plan.Bind(s).Count(ctx, nil)
				require.NoError(t, err)
				require.EqualValues(t, 4, n)
				return nil
			})
			require.NoError(t, err)
			err = runRowPlan(t, db, pool, driver, f.reg.SQLAuthority(), f.role, 1, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				_, err := plan.Bind(s).Page(ctx, q.RowChoice{}, q.BindValue(boundSlot, int64(1)))
				return err
			})
			require.Error(t, err)
			require.Equal(t, "42501", q.SQLState(err))
			_, err = db.ExecContext(t.Context(), "GRANT SELECT(label) ON "+f.schema+".items TO "+f.role)
			require.NoError(t, err)
			grouped := q.DefineRead(out, "group_minimal", q.Select(q.Project(q.Output(out, "label", q.Text()), f.label.Expr()), q.Project(q.Output(out, "hits", q.Int8()), q.CountOf(f.nid.Expr()))).From(f.items).LeftJoin(f.notes, q.Eq(f.parent.Expr(), f.id.Expr())).GroupBy(f.label.Expr()).WithList(q.ReadListSpec{ExactCount: true}))
			groupShape := out.Definition().StatementShapes["group_minimal.scoped.list.count"]
			for _, grant := range groupShape.Privileges {
				if grant.Table == `"`+f.schema+`"."notes"` {
					require.NotContains(t, grant.Columns, "id")
				}
			}
			_, err = db.ExecContext(t.Context(), "REVOKE SELECT(id) ON "+f.schema+".notes FROM "+f.role)
			require.NoError(t, err)
			err = runRowPlan(t, db, pool, driver, f.reg.SQLAuthority(), f.role, 1, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				n, err := grouped.Bind(s).Count(ctx, nil)
				require.NoError(t, err)
				require.EqualValues(t, 3, n)
				return nil
			})
			require.NoError(t, err)

		})
	}
}
