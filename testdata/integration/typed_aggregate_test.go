//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	t "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

type aggregateItem struct {
	ID    int32   `db:"id,key"`
	Label *string `db:"label"`
	Rank  int32   `db:"rank"`
	Flag  *bool   `db:"flag"`
}

type aggregateResult struct {
	Count int64            `db:"count"`
	Text  *string          `db:"text"`
	IDs   []int32          `db:"ids,array"`
	JSON  *json.RawMessage `db:"document,json"`
	JSONB *json.RawMessage `db:"binary_document,json"`
	Any   *bool            `db:"any"`
	All   *bool            `db:"all"`
}

type aggregateGroup struct {
	Missing bool  `db:"missing"`
	Count   int64 `db:"count"`
}

type aggregateNote struct {
	ID   int32 `db:"id,key"`
	Item int32 `db:"item_id"`
	Rank int32 `db:"rank"`
}

type aggregateDistinct struct {
	Text  *string `db:"text"`
	Ranks []int32 `db:"ranks,array"`
	Any   *bool   `db:"any"`
	All   *bool   `db:"all"`
}

func TestAggregateClausesBothDrivers(test *testing.T) {
	db, pool := databases(test)
	for _, driver := range []string{"stdsql", "pgx"} {
		test.Run(driver, func(test *testing.T) {
			ctx := test.Context()
			ns := fmt.Sprintf("aggregate_%d", time.Now().UnixNano())
			role := ns + "_reader"
			exec := func(query string) { test.Helper(); _, err := db.ExecContext(ctx, query); require.NoError(test, err) }
			exec("CREATE ROLE " + role + " NOBYPASSRLS")
			test.Cleanup(func() { _, err := db.Exec("DROP ROLE " + role); require.NoError(test, err) })
			exec("CREATE SCHEMA " + ns)
			test.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + ns + " CASCADE"); require.NoError(test, err) })
			exec("CREATE TABLE " + ns + ".items (id integer PRIMARY KEY, label text, rank integer NOT NULL, flag boolean)")
			exec("INSERT INTO " + ns + ".items VALUES (1,'a',3,true),(2,'a',1,false),(3,'b',7,NULL),(4,NULL,9,NULL),(5,NULL,0,false)")
			exec("CREATE TABLE " + ns + ".notes (id integer PRIMARY KEY, item_id integer NOT NULL, rank integer NOT NULL)")
			exec("INSERT INTO " + ns + ".notes VALUES (11,1,3),(12,1,1),(13,2,7)")
			exec("GRANT USAGE ON SCHEMA " + ns + " TO " + role)
			exec("GRANT SELECT(id,label,rank,flag) ON " + ns + ".items TO " + role)
			exec("GRANT SELECT(id,item_id,rank) ON " + ns + ".notes TO " + role)
			cfg := t.RegistryConfig[itemScope]{Deny: t.DenyLegacySQL, ArrayColumn: pgxdriver.ArrayColumn}
			if driver == "stdsql" {
				cfg.ArrayColumn, cfg.ArrayArg = pqdriver.ArrayColumn, pqdriver.Array
			}
			reg := t.NewRegistry(cfg)
			table := t.DefinePlanSource(reg, t.TableSpec[aggregateItem, int32]{Name: ns + ".items", RelationScope: t.Global()})
			src := plan.MapTable(table).As("i")
			id, rank := plan.FieldOf(src.Rel(), "id", pg.Int4()), plan.FieldOf(src.Rel(), "rank", pg.Int4())
			label, flag := plan.FieldOf(src.Rel(), "label", pg.Text()), plan.FieldOf(src.Rel(), "flag", pg.Bool())
			floor, delimiter := plan.Input("floor", pg.Int4()), plan.Input("delimiter", pg.Text())
			condition := plan.Ge(rank.Expr(), floor.Expr())
			out := t.DefineView(reg, t.ViewSpec[aggregateResult]{Name: "entries"})
			project := []plan.Projection{
				plan.Project(plan.Output(out, "count", pg.Int8()), plan.FilterWhere(plan.CountDistinct(label.Expr()), condition)),
				plan.Project(plan.Output(out, "text", pg.Text()), plan.FilterWhere(plan.AggregateOrder(plan.StringAgg(label.Expr(), delimiter.Expr()), plan.Desc(rank), plan.Asc(id)), condition)),
				plan.Project(plan.Output(out, "ids", pg.ArrayOf(pg.Int4())), plan.FilterWhere(plan.AggregateOrder(plan.ArrayAgg(id.Expr()), plan.Desc(rank), plan.Asc(id)), condition)),
				plan.Project(plan.Output(out, "document", pg.JSON[json.RawMessage]()), plan.FilterWhere(plan.AggregateOrder(plan.JSONAgg(pg.JSON[json.RawMessage](), label.Expr()), plan.Desc(rank), plan.Asc(id)), condition)),
				plan.Project(plan.Output(out, "binary_document", pg.JSONB[json.RawMessage]()), plan.FilterWhere(plan.AggregateOrder(plan.JSONBAgg(pg.JSONB[json.RawMessage](), label.Expr()), plan.Desc(rank), plan.Asc(id)), condition)),
				plan.Project(plan.Output(out, "any", pg.Bool()), plan.FilterWhere(plan.BoolOr(flag.Expr()), condition)),
				plan.Project(plan.Output(out, "all", pg.Bool()), plan.FilterWhere(plan.BoolAnd(flag.Expr()), condition)),
			}
			single := plan.DefineRows(out, "totals", src, plan.RowsSpec{}, project...)
			explicit := plan.DefineRead(out, "explicit", plan.Select(project...).From(src))
			groupOut := t.DefineView(reg, t.ViewSpec[aggregateGroup]{Name: "groups"})
			key := plan.IsNull(label.Expr())
			groups := []plan.Projection{plan.Project(plan.Output(groupOut, "missing", pg.Bool()), key), plan.Project(plan.Output(groupOut, "count", pg.Int8()), plan.CountAll())}
			grouped := plan.DefineRows(groupOut, "pages", src, plan.RowsSpec{GroupBy: []plan.RowColumn{plan.GroupExpr(key)}, Orders: []plan.RowOrder{{Name: "count", Terms: []plan.RowTerm{plan.DescExpr(plan.CountAll()), plan.AscExpr(key)}}}, Pages: plan.RowPages{Unpaged: true, Keyset: true, MaxLimit: 2}}, groups...)
			minimum := plan.Input("minimum", pg.Int8())
			groupRead := plan.DefineRead(groupOut, "read", plan.Select(groups...).From(src).GroupBy(key).Having(plan.Ge(plan.CountAll(), minimum.Expr())).OrderBy(plan.AscExpr(key)))
			winners := plan.DefineTableRead(src, "winners", plan.SelectColumns(label.Column(), id.Column(), rank.Column()), plan.Select().DistinctOn(label.Expr()).OrderBy(plan.Asc(label).NullsFirst(), plan.Desc(rank), plan.Asc(id)))
			distinct := plan.DefineTableRead(src, "labels", plan.SelectColumns(label.Column()), plan.Select().Distinct().OrderBy(plan.Asc(label).NullsFirst()))
			var pages []*plan.RowsPlan[aggregateItem, itemScope]
			for i, term := range []plan.RowTerm{plan.AscExpr(label.Expr()).NullsFirst(), plan.AscExpr(label.Expr()).NullsLast(), plan.DescExpr(label.Expr()).NullsFirst(), plan.DescExpr(label.Expr()).NullsLast()} {
				pages = append(pages, plan.DefineTableRows(src, fmt.Sprintf("page_%d", i), plan.RowsSpec{Orders: []plan.RowOrder{{Name: "key", Terms: []plan.RowTerm{term, plan.Asc(id)}}}, Pages: plan.RowPages{Unpaged: true, Keyset: true, MaxLimit: 2}}, plan.SelectColumns(label.Column(), id.Column())))
			}
			notes := t.DefinePlanSource(reg, t.TableSpec[aggregateNote, int32]{Name: ns + ".notes", RelationScope: t.Global()})
			right := plan.MapTable(notes).As("n")
			nid, parent, nrank := plan.FieldOf(right.Rel(), "id", pg.Int4()), plan.FieldOf(right.Rel(), "item_id", pg.Int4()), plan.FieldOf(right.Rel(), "rank", pg.Int4())
			joined := plan.DefineRead(groupOut, "joined", plan.Select(groups[0], plan.Project(plan.Output(groupOut, "count", pg.Int8()), plan.FilterWhere(plan.CountDistinct(nid.Expr()), plan.Ge(nrank.Expr(), floor.Expr())))).From(src).LeftJoin(right, plan.Eq(parent.Expr(), id.Expr())).GroupBy(key).OrderBy(plan.AscExpr(key)))
			distinctOut := t.DefineView(reg, t.ViewSpec[aggregateDistinct]{Name: "notes"})
			distinctInputs := plan.DefineRead(distinctOut, "arguments", plan.Select(
				plan.Project(plan.Output(distinctOut, "text", pg.Text()), plan.AggregateOrder(plan.DistinctInputs(plan.StringAgg(label.Expr(), delimiter.Expr())), plan.Desc(label))),
				plan.Project(plan.Output(distinctOut, "ranks", pg.ArrayOf(pg.Int4())), plan.AggregateOrder(plan.DistinctInputs(plan.ArrayAgg(rank.Expr())), plan.Desc(rank))),
				plan.Project(plan.Output(distinctOut, "any", pg.Bool()), plan.DistinctInputs(plan.BoolOr(flag.Expr()))),
				plan.Project(plan.Output(distinctOut, "all", pg.Bool()), plan.DistinctInputs(plan.BoolAnd(flag.Expr()))),
			).From(src))
			verifyAggregateDefinitions(test, db, role, reg)
			run := func(body rowCallback) {
				test.Helper()
				require.NoError(test, runRowPlan(test, db, pool, driver, reg.SQLAuthority(), role, 1, t.LogConfig{}, body))
			}
			run(func(ctx context.Context, session t.Session[itemScope]) error {
				bindings := []plan.Binding{plan.BindValue(floor, 3), plan.BindValue(delimiter, ",")}
				rows, total, err := single.Bind(session).List(ctx, plan.RowChoice{}, bindings...)
				require.NoError(test, err)
				require.EqualValues(test, 1, total)
				require.Len(test, rows, 1)
				require.EqualValues(test, 2, rows[0].Count)
				require.Equal(test, "b,a", *rows[0].Text)
				require.Equal(test, []int32{4, 3, 1}, rows[0].IDs)
				require.JSONEq(test, `[null,"b","a"]`, string(*rows[0].JSON))
				require.JSONEq(test, `[null,"b","a"]`, string(*rows[0].JSONB))
				require.True(test, *rows[0].Any)
				require.True(test, *rows[0].All)
				repeated, err := explicit.Bind(session).Find(ctx, bindings...)
				require.NoError(test, err)
				require.Equal(test, rows, repeated)
				empty, err := explicit.Bind(session).Find(ctx, plan.BindValue(floor, 100), plan.BindValue(delimiter, ","))
				require.NoError(test, err)
				require.Equal(test, []aggregateResult{{}}, empty)
				all, total, err := grouped.Bind(session).List(ctx, plan.RowChoice{Order: "count"})
				require.NoError(test, err)
				require.EqualValues(test, 2, total)
				require.Equal(test, []aggregateGroup{{false, 3}, {true, 2}}, all)
				readGroups, err := groupRead.Bind(session).Find(ctx, plan.BindValue(minimum, 2))
				require.NoError(test, err)
				require.Equal(test, all, readGroups)
				cursor, err := grouped.CursorFrom("count", all[0])
				require.NoError(test, err)
				next, total, err := grouped.Bind(session).List(ctx, plan.RowChoice{Order: "count", Page: plan.AfterPage, Limit: 1, After: cursor})
				require.NoError(test, err)
				require.EqualValues(test, 2, total)
				require.Equal(test, all[1:], next)
				winnerRows, err := winners.Bind(session).Find(ctx)
				require.NoError(test, err)
				require.Equal(test, []aggregateItem{{ID: 4, Rank: 9}, {ID: 1, Label: new("a"), Rank: 3}, {ID: 3, Label: new("b"), Rank: 7}}, winnerRows)
				labels, err := distinct.Bind(session).Find(ctx)
				require.NoError(test, err)
				require.Equal(test, []aggregateItem{{}, {Label: new("a")}, {Label: new("b")}}, labels)
				joinedRows, err := joined.Bind(session).Find(ctx, plan.BindValue(floor, 3))
				require.NoError(test, err)
				require.Equal(test, []aggregateGroup{{false, 2}, {true, 0}}, joinedRows)
				unique, err := distinctInputs.Bind(session).Find(ctx, plan.BindValue(delimiter, ","))
				require.NoError(test, err)
				require.Equal(test, []aggregateDistinct{{Text: new("b,a"), Ranks: []int32{9, 7, 3, 1, 0}, Any: new(true), All: new(false)}}, unique)
				for _, page := range pages {
					all, err := page.Bind(session).Find(ctx, plan.RowChoice{Order: "key"})
					require.NoError(test, err)
					var traversed []aggregateItem
					choice := plan.RowChoice{Order: "key", Page: plan.FirstPage, Limit: 1}
					for range len(all) + 1 {
						chunk, err := page.Bind(session).Find(ctx, choice)
						require.NoError(test, err)
						if len(chunk) == 0 {
							break
						}
						traversed = append(traversed, chunk...)
						choice.After, err = page.CursorFrom("key", chunk[0])
						require.NoError(test, err)
						manual := plan.NullAtExpr(label.Expr())
						if chunk[0].Label != nil {
							manual = plan.AtExpr(label.Expr(), *chunk[0].Label)
						}
						boundary, err := page.Cursor("key", manual, plan.At(id, chunk[0].ID))
						require.NoError(test, err)
						require.Equal(test, boundary, choice.After)
						choice.Page = plan.AfterPage
					}
					require.Equal(test, all, traversed)
				}
				return nil
			})
			exec("REVOKE SELECT(rank) ON " + ns + ".items FROM " + role)
			run(func(ctx context.Context, session t.Session[itemScope]) error {
				n, err := single.Bind(session).Count(ctx, nil)
				require.NoError(test, err)
				require.EqualValues(test, 1, n)
				return nil
			})
			err := runRowPlan(test, db, pool, driver, reg.SQLAuthority(), role, 1, t.LogConfig{}, func(ctx context.Context, session t.Session[itemScope]) error {
				_, err := explicit.Bind(session).Find(ctx, plan.BindValue(floor, 3), plan.BindValue(delimiter, ","))
				return err
			})
			require.Equal(test, "42501", t.SQLState(err))
		})
	}
}

func verifyAggregateDefinitions(test *testing.T, db *sql.DB, role string, reg *t.Registry[itemScope]) {
	test.Helper()
	text := sqlcheck.RenderAllSnapshot(reg.Defined(), reg.DefinedViews())
	defs, err := sqlcheck.ParseSnapshot(text)
	require.NoError(test, err)
	require.Equal(test, text, sqlcheck.RenderSnapshot(defs))
	work := sqlcheck.RoleStatements{}
	work.AddDefinitions(role, defs...)
	needs, err := work.Readiness(role)
	require.NoError(test, err)
	missing, err := sqlcheck.CheckReadiness(test.Context(), db, role, needs)
	require.NoError(test, err)
	require.Empty(test, missing)
	findings, err := sqlcheck.VerifyRoleStatements(test.Context(), db, work)
	require.NoError(test, err)
	require.Empty(test, findings)
}
