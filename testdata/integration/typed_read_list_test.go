//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

type listItem struct {
	ID      int64     `db:"id,key"`
	Scope   itemScope `db:"scope_id,scope"`
	Label   *string   `db:"label"`
	Removed bool      `db:"removed,deleted"`
}
type listNote struct {
	ID      int64     `db:"id,key"`
	Scope   itemScope `db:"scope_id,scope"`
	Item    int64     `db:"item_id"`
	Rank    *int32    `db:"rank"`
	Removed bool      `db:"removed,deleted"`
}
type listEntry struct {
	Item int64  `db:"item"`
	Note *int64 `db:"note"`
	Rank *int32 `db:"rank"`
}

type listFixture struct {
	reg             *q.Registry[itemScope]
	schema, role    string
	layout          *tplan.TableLayout[listItem, itemScope]
	items           tplan.RowSource[listItem, itemScope]
	notes           tplan.RowSource[listNote, itemScope]
	id, nid, parent tplan.Field[int64]
	rank            tplan.Field[int32]
	label           tplan.Field[string]
	out             *q.View[listEntry, itemScope]
	query           tplan.ReadSelect
}

func newListFixture(t *testing.T, db *sql.DB, driver string) listFixture {
	t.Helper()
	ns := fmt.Sprintf("read_list_%d", time.Now().UnixNano())
	role := ns + "_reader"
	exec := func(text string) { t.Helper(); _, err := db.ExecContext(t.Context(), text); require.NoError(t, err) }
	exec("CREATE ROLE " + role + " NOBYPASSRLS")
	t.Cleanup(func() { _, err := db.Exec("DROP ROLE " + role); require.NoError(t, err) })
	exec("CREATE SCHEMA " + ns)
	t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + ns + " CASCADE"); require.NoError(t, err) })
	exec("CREATE TABLE " + ns + ".items (id bigint PRIMARY KEY, scope_id bigint NOT NULL, label text, removed bool NOT NULL)")
	exec("CREATE TABLE " + ns + ".notes (id bigint PRIMARY KEY, scope_id bigint NOT NULL, item_id bigint NOT NULL, rank integer, removed bool NOT NULL)")
	exec("INSERT INTO " + ns + ".items VALUES (1,1,'a',false),(2,1,'b',false),(3,1,NULL,false),(4,1,'b',false),(5,2,'x',false),(6,1,'hidden',true)")
	exec("INSERT INTO " + ns + ".notes VALUES (11,1,1,3,false),(12,1,1,1,false),(13,1,2,-2,false),(14,1,2,NULL,false),(15,2,3,4,false),(16,1,3,5,true)")
	exec("GRANT USAGE ON SCHEMA " + ns + " TO " + role)
	exec("GRANT SELECT (id,scope_id,label,removed) ON " + ns + ".items TO " + role)
	exec("GRANT SELECT (id,scope_id,item_id,rank,removed) ON " + ns + ".notes TO " + role)
	cfg := q.RegistryConfig[itemScope]{Deny: q.DenyLegacySQL}
	if driver == "stdsql" {
		cfg.ArrayArg = pqdriver.Array
	}
	reg := q.NewRegistry(cfg)
	layout := tplan.MapTable(q.DefinePlanSource(reg, q.TableSpec[listItem, int64]{Name: ns + ".items"}), tplan.ColumnType("scope_id", q.CatalogType[itemScope]("pg_catalog", "int8", nil)))
	items := layout.As("i")
	notes := tplan.MapTable(q.DefinePlanSource(reg, q.TableSpec[listNote, int64]{Name: ns + ".notes"}), tplan.ColumnType("scope_id", q.CatalogType[itemScope]("pg_catalog", "int8", nil))).As("n")
	id, nid := tplan.FieldOf(items.Rel(), "id", tplan.Int8()), tplan.FieldOf(notes.Rel(), "id", tplan.Int8())
	parent, rank := tplan.FieldOf(notes.Rel(), "item_id", tplan.Int8()), tplan.FieldOf(notes.Rel(), "rank", tplan.Int4())
	out := q.DefineView(reg, q.ViewSpec[listEntry]{Name: "entries"})
	query := tplan.Select(tplan.Project(tplan.Output(out, "item", tplan.Int8()), id.Expr()), tplan.Project(tplan.Output(out, "note", tplan.Int8()), nid.Expr()), tplan.Project(tplan.Output(out, "rank", tplan.Int4()), rank.Expr())).From(items).LeftJoin(notes, tplan.Eq(parent.Expr(), id.Expr()))
	return listFixture{layout: layout, reg: reg, schema: ns, role: role, items: items, notes: notes, id: id, nid: nid, parent: parent, rank: rank, label: tplan.FieldOf(items.Rel(), "label", tplan.Text()), out: out, query: query}
}

func verifyListSnapshot(t *testing.T, db *sql.DB, f listFixture) {
	t.Helper()
	snapshot := sqlcheck.RenderAllSnapshot(f.reg.Defined(), f.reg.DefinedViews())
	path := filepath.Join(t.TempDir(), "statements.sql")
	require.NoError(t, os.WriteFile(path, []byte(snapshot), 0600))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	defs, err := sqlcheck.ParseSnapshot(string(data))
	require.NoError(t, err)
	work := sqlcheck.RoleStatements{}
	work.AddDefinitions(f.role, defs...)
	needs, err := work.Readiness(f.role)
	require.NoError(t, err)
	missing, err := sqlcheck.CheckReadiness(t.Context(), db, f.role, needs)
	require.NoError(t, err)
	require.Empty(t, missing)
	failures, err := sqlcheck.VerifyRoleStatements(t.Context(), db, work)
	require.NoError(t, err)
	require.Empty(t, failures)
}

func TestReadListJoinedPagesBothDrivers(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			f := newListFixture(t, db, driver)
			ids, minimum := tplan.Input("ids", q.ArrayOf(tplan.Int8())), tplan.Input("minimum", tplan.Int4())
			orders := []tplan.RowOrder{{Name: "rank", Terms: []tplan.RowTerm{tplan.Desc(f.rank).NullsLast(), tplan.Asc(f.id), tplan.Asc(f.nid).NullsLast()}}, {Name: "reverse", Terms: []tplan.RowTerm{tplan.Asc(f.rank).NullsFirst(), tplan.Desc(f.id), tplan.Desc(f.nid).NullsFirst()}}}
			spec := tplan.ReadListSpec{Optional: []tplan.RowPredicate{{Name: "selected", When: tplan.Any(f.id.Expr(), ids.Expr())}, {Name: "ranked", When: tplan.Ge(f.rank.Expr(), minimum.Expr())}}, Orders: orders, Pages: tplan.RowPages{Unpaged: true, Offset: true, Keyset: true, MaxLimit: 10, MaxOffset: 20}, ExactCount: true}
			plan := tplan.DefineRead(f.out, "joined", f.query.WithList(spec))
			verifyListSnapshot(t, db, f)
			err := runRowPlan(t, db, pool, driver, f.reg.SQLAuthority(), f.role, 1, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				bound := plan.Bind(s)
				for _, order := range orders {
					all, total, err := bound.List(ctx, tplan.RowChoice{Order: order.Name})
					require.NoError(t, err)
					require.EqualValues(t, 6, total)
					require.Len(t, all, 6)
					choice := tplan.RowChoice{Order: order.Name, Page: tplan.FirstPage, Limit: 2}
					var combined []listEntry
					for i := 0; i < 10; i++ {
						page, total, err := bound.List(ctx, choice)
						require.NoError(t, err)
						require.EqualValues(t, 6, total)
						if len(page) == 0 {
							break
						}
						combined = append(combined, page...)
						cursor, err := plan.CursorFrom(order.Name, page[len(page)-1])
						require.NoError(t, err)
						choice.Page, choice.After = tplan.AfterPage, cursor
					}
					require.Equal(t, all, combined)
					page, err := bound.Page(ctx, tplan.RowChoice{Order: order.Name, Page: tplan.OffsetPage, Limit: 2, Offset: 4})
					require.NoError(t, err)
					require.Equal(t, all[4:], page)
				}
				selected := tplan.BindValue(ids, []int64{1, 3})
				page, total, err := bound.List(ctx, tplan.RowChoice{Order: "rank", Filters: []string{"selected"}, Page: tplan.OffsetPage, Limit: 2}, selected)
				require.NoError(t, err)
				require.Len(t, page, 2)
				require.EqualValues(t, 3, total)
				total, err = bound.Count(ctx, []string{"ranked"}, tplan.BindValue(minimum, int32(0)))
				require.NoError(t, err)
				require.EqualValues(t, 2, total)
				for _, empty := range [][]int64{nil, {}} {
					page, total, err = bound.List(ctx, tplan.RowChoice{Order: "rank", Filters: []string{"selected"}}, tplan.BindValue(ids, empty))
					require.NoError(t, err)
					require.Empty(t, page)
					require.Zero(t, total)
				}
				for _, bad := range []tplan.RowChoice{{Order: "rank", Page: tplan.FirstPage, Limit: 11}, {Order: "rank", Page: tplan.OffsetPage, Limit: 1, Offset: 21}, {Order: "rank", Page: tplan.AfterPage, Limit: 1}, {Order: "rank", Filters: []string{"selected", "selected"}}, {Order: "missing"}} {
					_, err := bound.Page(ctx, bad, selected)
					require.Error(t, err)
				}
				return nil
			})
			require.NoError(t, err)
		})
	}
}
