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
			src := t.MapTable(table).As("i")
			id, rank := t.FieldOf(src.Rel(), "id", pg.Int4()), t.FieldOf(src.Rel(), "rank", pg.Int4())
			label, flag := t.FieldOf(src.Rel(), "label", pg.Text()), t.FieldOf(src.Rel(), "flag", pg.Bool())
			floor, delimiter := t.Input("floor", pg.Int4()), t.Input("delimiter", pg.Text())
			condition := t.Ge(rank.Expr(), floor.Expr())
			out := t.DefineView(reg, t.ViewSpec[aggregateResult]{Name: "entries"})
			project := []t.Projection{
				t.Project(t.Output(out, "count", pg.Int8()), t.FilterWhere(t.CountDistinct(label.Expr()), condition)),
				t.Project(t.Output(out, "text", pg.Text()), t.FilterWhere(t.AggregateOrder(t.StringAgg(label.Expr(), delimiter.Expr()), t.Desc(rank), t.Asc(id)), condition)),
				t.Project(t.Output(out, "ids", pg.ArrayOf(pg.Int4())), t.FilterWhere(t.AggregateOrder(t.ArrayAgg(id.Expr()), t.Desc(rank), t.Asc(id)), condition)),
				t.Project(t.Output(out, "document", pg.JSON[json.RawMessage]()), t.FilterWhere(t.AggregateOrder(t.JSONAgg(pg.JSON[json.RawMessage](), label.Expr()), t.Desc(rank), t.Asc(id)), condition)),
				t.Project(t.Output(out, "binary_document", pg.JSONB[json.RawMessage]()), t.FilterWhere(t.AggregateOrder(t.JSONBAgg(pg.JSONB[json.RawMessage](), label.Expr()), t.Desc(rank), t.Asc(id)), condition)),
				t.Project(t.Output(out, "any", pg.Bool()), t.FilterWhere(t.BoolOr(flag.Expr()), condition)),
				t.Project(t.Output(out, "all", pg.Bool()), t.FilterWhere(t.BoolAnd(flag.Expr()), condition)),
			}
			single := t.DefineRows(out, "totals", src, t.RowsSpec{}, project...)
			explicit := t.DefineRead(out, "explicit", t.Select(project...).From(src))
			groupOut := t.DefineView(reg, t.ViewSpec[aggregateGroup]{Name: "groups"})
			key := t.IsNull(label.Expr())
			groups := []t.Projection{t.Project(t.Output(groupOut, "missing", pg.Bool()), key), t.Project(t.Output(groupOut, "count", pg.Int8()), t.CountAll())}
			grouped := t.DefineRows(groupOut, "pages", src, t.RowsSpec{GroupBy: []t.RowColumn{t.GroupExpr(key)}, Orders: []t.RowOrder{{Name: "count", Terms: []t.RowTerm{t.DescExpr(t.CountAll()), t.AscExpr(key)}}}, Pages: t.RowPages{Unpaged: true, Keyset: true, MaxLimit: 2}}, groups...)
			minimum := t.Input("minimum", pg.Int8())
			groupRead := t.DefineRead(groupOut, "read", t.Select(groups...).From(src).GroupBy(key).Having(t.Ge(t.CountAll(), minimum.Expr())).OrderBy(t.AscExpr(key)))
			winners := t.DefineTableRead(src, "winners", t.SelectColumns(label.Column(), id.Column(), rank.Column()), t.Select().DistinctOn(label.Expr()).OrderBy(t.Asc(label).NullsFirst(), t.Desc(rank), t.Asc(id)))
			distinct := t.DefineTableRead(src, "labels", t.SelectColumns(label.Column()), t.Select().Distinct().OrderBy(t.Asc(label).NullsFirst()))
			var pages []*t.RowsPlan[aggregateItem, itemScope]
			for i, term := range []t.RowTerm{t.AscExpr(label.Expr()).NullsFirst(), t.AscExpr(label.Expr()).NullsLast(), t.DescExpr(label.Expr()).NullsFirst(), t.DescExpr(label.Expr()).NullsLast()} {
				pages = append(pages, t.DefineTableRows(src, fmt.Sprintf("page_%d", i), t.RowsSpec{Orders: []t.RowOrder{{Name: "key", Terms: []t.RowTerm{term, t.Asc(id)}}}, Pages: t.RowPages{Unpaged: true, Keyset: true, MaxLimit: 2}}, t.SelectColumns(label.Column(), id.Column())))
			}
			notes := t.DefinePlanSource(reg, t.TableSpec[aggregateNote, int32]{Name: ns + ".notes", RelationScope: t.Global()})
			right := t.MapTable(notes).As("n")
			nid, parent, nrank := t.FieldOf(right.Rel(), "id", pg.Int4()), t.FieldOf(right.Rel(), "item_id", pg.Int4()), t.FieldOf(right.Rel(), "rank", pg.Int4())
			joined := t.DefineRead(groupOut, "joined", t.Select(groups[0], t.Project(t.Output(groupOut, "count", pg.Int8()), t.FilterWhere(t.CountDistinct(nid.Expr()), t.Ge(nrank.Expr(), floor.Expr())))).From(src).LeftJoin(right, t.Eq(parent.Expr(), id.Expr())).GroupBy(key).OrderBy(t.AscExpr(key)))
			distinctOut := t.DefineView(reg, t.ViewSpec[aggregateDistinct]{Name: "notes"})
			distinctInputs := t.DefineRead(distinctOut, "arguments", t.Select(
				t.Project(t.Output(distinctOut, "text", pg.Text()), t.AggregateOrder(t.DistinctInputs(t.StringAgg(label.Expr(), delimiter.Expr())), t.Desc(label))),
				t.Project(t.Output(distinctOut, "ranks", pg.ArrayOf(pg.Int4())), t.AggregateOrder(t.DistinctInputs(t.ArrayAgg(rank.Expr())), t.Desc(rank))),
				t.Project(t.Output(distinctOut, "any", pg.Bool()), t.DistinctInputs(t.BoolOr(flag.Expr()))),
				t.Project(t.Output(distinctOut, "all", pg.Bool()), t.DistinctInputs(t.BoolAnd(flag.Expr()))),
			).From(src))
			verifyAggregateDefinitions(test, db, role, reg)
			run := func(body rowCallback) {
				test.Helper()
				require.NoError(test, runRowPlan(test, db, pool, driver, reg.SQLAuthority(), role, 1, t.LogConfig{}, body))
			}
			run(func(ctx context.Context, session t.Session[itemScope]) error {
				bindings := []t.Binding{t.BindValue(floor, 3), t.BindValue(delimiter, ",")}
				rows, total, err := single.Bind(session).List(ctx, t.RowChoice{}, bindings...)
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
				empty, err := explicit.Bind(session).Find(ctx, t.BindValue(floor, 100), t.BindValue(delimiter, ","))
				require.NoError(test, err)
				require.Equal(test, []aggregateResult{{}}, empty)
				all, total, err := grouped.Bind(session).List(ctx, t.RowChoice{Order: "count"})
				require.NoError(test, err)
				require.EqualValues(test, 2, total)
				require.Equal(test, []aggregateGroup{{false, 3}, {true, 2}}, all)
				readGroups, err := groupRead.Bind(session).Find(ctx, t.BindValue(minimum, 2))
				require.NoError(test, err)
				require.Equal(test, all, readGroups)
				cursor, err := grouped.CursorFrom("count", all[0])
				require.NoError(test, err)
				next, total, err := grouped.Bind(session).List(ctx, t.RowChoice{Order: "count", Page: t.AfterPage, Limit: 1, After: cursor})
				require.NoError(test, err)
				require.EqualValues(test, 2, total)
				require.Equal(test, all[1:], next)
				winnerRows, err := winners.Bind(session).Find(ctx)
				require.NoError(test, err)
				require.Equal(test, []aggregateItem{{ID: 4, Rank: 9}, {ID: 1, Label: new("a"), Rank: 3}, {ID: 3, Label: new("b"), Rank: 7}}, winnerRows)
				labels, err := distinct.Bind(session).Find(ctx)
				require.NoError(test, err)
				require.Equal(test, []aggregateItem{{}, {Label: new("a")}, {Label: new("b")}}, labels)
				joinedRows, err := joined.Bind(session).Find(ctx, t.BindValue(floor, 3))
				require.NoError(test, err)
				require.Equal(test, []aggregateGroup{{false, 2}, {true, 0}}, joinedRows)
				unique, err := distinctInputs.Bind(session).Find(ctx, t.BindValue(delimiter, ","))
				require.NoError(test, err)
				require.Equal(test, []aggregateDistinct{{Text: new("b,a"), Ranks: []int32{9, 7, 3, 1, 0}, Any: new(true), All: new(false)}}, unique)
				for _, page := range pages {
					all, err := page.Bind(session).Find(ctx, t.RowChoice{Order: "key"})
					require.NoError(test, err)
					var traversed []aggregateItem
					choice := t.RowChoice{Order: "key", Page: t.FirstPage, Limit: 1}
					for range len(all) + 1 {
						chunk, err := page.Bind(session).Find(ctx, choice)
						require.NoError(test, err)
						if len(chunk) == 0 {
							break
						}
						traversed = append(traversed, chunk...)
						choice.After, err = page.CursorFrom("key", chunk[0])
						require.NoError(test, err)
						manual := t.NullAtExpr(label.Expr())
						if chunk[0].Label != nil {
							manual = t.AtExpr(label.Expr(), *chunk[0].Label)
						}
						boundary, err := page.Cursor("key", manual, t.At(id, chunk[0].ID))
						require.NoError(test, err)
						require.Equal(test, boundary, choice.After)
						choice.Page = t.AfterPage
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
				_, err := explicit.Bind(session).Find(ctx, t.BindValue(floor, 3), t.BindValue(delimiter, ","))
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
