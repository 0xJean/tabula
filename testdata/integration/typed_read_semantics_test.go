//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	q "github.com/0xJean/tabula"
	tplan "github.com/0xJean/tabula/plan"
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
			order := tplan.RowOrder{Name: "rank", Terms: []tplan.RowTerm{tplan.Desc(f.rank).NullsLast(), tplan.Asc(f.id)}}
			winners := tplan.DefineRead(f.out, "winners", f.query.DistinctOn(f.id.Expr()).OrderBy(tplan.Asc(f.id), tplan.Desc(f.rank).NullsLast(), tplan.Asc(f.nid)).WithForms(tplan.ReadForms{Exact: true}).
				WithList(tplan.ReadListSpec{Orders: []tplan.RowOrder{order}, Pages: tplan.RowPages{Unpaged: true, Keyset: true, MaxLimit: 3}, ExactCount: true}))
			nullable := tplan.DefineRead(f.out, "nullable_keys", f.query.DistinctOn(f.label.Expr()).OrderBy(tplan.Asc(f.label), tplan.Asc(f.id), tplan.Asc(f.nid)).WithList(tplan.ReadListSpec{ExactCount: true}))
			out := q.DefineView(f.reg, q.ViewSpec[listGroup]{Name: "groups"})
			hits := tplan.CountOf(f.nid.Expr())
			atLeast := tplan.Input("at_least", tplan.Int8())
			grouped := tplan.DefineRead(out, "grouped", tplan.Select(tplan.Project(tplan.Output(out, "label", tplan.Text()), f.label.Expr()), tplan.Project(tplan.Output(out, "hits", tplan.Int8()), hits)).
				From(f.items).LeftJoin(f.notes, tplan.Eq(f.parent.Expr(), f.id.Expr())).GroupBy(f.label.Expr()).Having(tplan.Ge(hits, atLeast.Expr())).
				WithList(tplan.ReadListSpec{Orders: []tplan.RowOrder{{Name: "label", Terms: []tplan.RowTerm{tplan.Asc(f.label).NullsFirst()}}}, Pages: tplan.RowPages{Unpaged: true, Keyset: true, MaxLimit: 2}, ExactCount: true}))
			type labelRow struct {
				Label *string `db:"label"`
			}
			labels := q.DefineView(f.reg, q.ViewSpec[labelRow]{Name: "labels"})
			distinct := tplan.DefineRead(labels, "distinct", tplan.Select(tplan.Project(tplan.Output(labels, "label", tplan.Text()), f.label.Expr())).From(f.items).Distinct().WithList(tplan.ReadListSpec{
				Orders: []tplan.RowOrder{{Name: "label", Terms: []tplan.RowTerm{tplan.Desc(f.label).NullsLast()}}}, Pages: tplan.RowPages{Unpaged: true, Keyset: true, MaxLimit: 1}, ExactCount: true}))
			type totalRow struct {
				Total int64 `db:"total"`
			}
			totals := q.DefineView(f.reg, q.ViewSpec[totalRow]{Name: "totals"})
			ids := tplan.Input("ids", q.ArrayOf(tplan.Int8()))
			aggregate := tplan.DefineRead(totals, "aggregate", tplan.Select(tplan.Project(tplan.Output(totals, "total", tplan.Int8()), tplan.CountAll())).From(f.items).Where(tplan.Any(f.id.Expr(), ids.Expr())).WithList(tplan.ReadListSpec{ExactCount: true}))

			orderOnly := tplan.DefineRead(totals, "order_aggregate", tplan.Select(tplan.Project(tplan.Output(totals, "total", tplan.Int8()), atLeast.Expr())).From(f.items).Where(tplan.Any(f.id.Expr(), ids.Expr())).OrderBy(tplan.AscExpr(tplan.CountAll())).WithList(tplan.ReadListSpec{Orders: []tplan.RowOrder{{Name: "constant", Terms: []tplan.RowTerm{tplan.AscExpr(atLeast.Expr())}}}, Pages: tplan.RowPages{Offset: true, MaxLimit: 1, MaxOffset: 2}, ExactCount: true}))
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
				all, total, err := winners.Bind(s).List(ctx, tplan.RowChoice{Order: "rank"})
				require.NoError(t, err)
				require.Equal(t, expected, all)
				require.EqualValues(t, 4, total)
				var combined []listEntry
				choice := tplan.RowChoice{Order: "rank", Page: tplan.FirstPage, Limit: 1}
				for range 6 {
					page, err := winners.Bind(s).Page(ctx, choice)
					require.NoError(t, err)
					if len(page) == 0 {
						break
					}
					combined = append(combined, page...)
					cursor, err := winners.CursorFrom("rank", page[0])
					require.NoError(t, err)
					choice.Page, choice.After = tplan.AfterPage, cursor
				}
				require.Equal(t, expected, combined) // no losing note becomes a second winner
				total, err = nullable.Bind(s).Count(ctx, nil)
				require.NoError(t, err)
				require.EqualValues(t, 3, total) // NULL is a group
				groups, n, err := grouped.Bind(s).List(ctx, tplan.RowChoice{Order: "label", Page: tplan.FirstPage, Limit: 1}, tplan.BindValue(atLeast, int64(0)))
				require.NoError(t, err)
				require.EqualValues(t, 3, n)
				require.Equal(t, []listGroup{{nil, 0}}, groups)
				cursor, err := grouped.CursorFrom("label", groups[0])
				require.NoError(t, err)
				groups, n, err = grouped.Bind(s).List(ctx, tplan.RowChoice{Order: "label", Page: tplan.AfterPage, Limit: 2, After: cursor}, tplan.BindValue(atLeast, int64(0)))
				require.NoError(t, err)
				require.EqualValues(t, 3, n)
				require.Equal(t, []listGroup{{new("a"), 2}, {new("b"), 2}}, groups)
				n, err = grouped.Bind(s).Count(ctx, nil, tplan.BindValue(atLeast, int64(3)))
				require.NoError(t, err)
				require.Zero(t, n)
				want := []labelRow{{new("b")}, {new("a")}, {nil}}
				var got []labelRow
				dc := tplan.RowChoice{Order: "label", Page: tplan.FirstPage, Limit: 1}
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
					dc.Page, dc.After = tplan.AfterPage, cursor
				}
				require.Equal(t, want, got)
				scalar, n, err := aggregate.Bind(s).List(ctx, tplan.RowChoice{}, tplan.BindValue(ids, []int64{}))
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				require.Equal(t, []totalRow{{0}}, scalar)
				scalar, n, err = orderOnly.Bind(s).List(ctx, tplan.RowChoice{Order: "constant", Page: tplan.OffsetPage, Limit: 1}, tplan.BindValue(ids, []int64{}), tplan.BindValue(atLeast, int64(7)))
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
			boundSlot := tplan.Input("projected", tplan.Int8())
			plan := tplan.DefineRead(out, "minimal", tplan.Select(tplan.Project(tplan.Output(out, "label", tplan.Text()), f.label.Expr()), tplan.Project(tplan.Output(out, "hits", tplan.Int8()), boundSlot.Expr())).From(f.items).WithList(tplan.ReadListSpec{ExactCount: true}))
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
				_, err := plan.Bind(s).Page(ctx, tplan.RowChoice{}, tplan.BindValue(boundSlot, int64(1)))
				return err
			})
			require.Error(t, err)
			require.Equal(t, "42501", q.SQLState(err))
			_, err = db.ExecContext(t.Context(), "GRANT SELECT(label) ON "+f.schema+".items TO "+f.role)
			require.NoError(t, err)
			grouped := tplan.DefineRead(out, "group_minimal", tplan.Select(tplan.Project(tplan.Output(out, "label", tplan.Text()), f.label.Expr()), tplan.Project(tplan.Output(out, "hits", tplan.Int8()), tplan.CountOf(f.nid.Expr()))).From(f.items).LeftJoin(f.notes, tplan.Eq(f.parent.Expr(), f.id.Expr())).GroupBy(f.label.Expr()).WithList(tplan.ReadListSpec{ExactCount: true}))
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
