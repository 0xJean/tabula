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
	layout          *q.TableLayout[listItem, itemScope]
	items           q.RowSource[listItem, itemScope]
	notes           q.RowSource[listNote, itemScope]
	id, nid, parent q.Field[int64]
	rank            q.Field[int32]
	label           q.Field[string]
	out             *q.View[listEntry, itemScope]
	query           q.ReadSelect
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
	layout := q.MapTable(q.DefinePlanSource(reg, q.TableSpec[listItem, int64]{Name: ns + ".items"}), q.ColumnType("scope_id", q.CatalogType[itemScope]("pg_catalog", "int8", nil)))
	items := layout.As("i")
	notes := q.MapTable(q.DefinePlanSource(reg, q.TableSpec[listNote, int64]{Name: ns + ".notes"}), q.ColumnType("scope_id", q.CatalogType[itemScope]("pg_catalog", "int8", nil))).As("n")
	id, nid := q.FieldOf(items.Rel(), "id", q.Int8()), q.FieldOf(notes.Rel(), "id", q.Int8())
	parent, rank := q.FieldOf(notes.Rel(), "item_id", q.Int8()), q.FieldOf(notes.Rel(), "rank", q.Int4())
	out := q.DefineView(reg, q.ViewSpec[listEntry]{Name: "entries"})
	query := q.Select(q.Project(q.Output(out, "item", q.Int8()), id.Expr()), q.Project(q.Output(out, "note", q.Int8()), nid.Expr()), q.Project(q.Output(out, "rank", q.Int4()), rank.Expr())).From(items).LeftJoin(notes, q.Eq(parent.Expr(), id.Expr()))
	return listFixture{layout: layout, reg: reg, schema: ns, role: role, items: items, notes: notes, id: id, nid: nid, parent: parent, rank: rank, label: q.FieldOf(items.Rel(), "label", q.Text()), out: out, query: query}
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
			ids, minimum := q.Input("ids", q.ArrayOf(q.Int8())), q.Input("minimum", q.Int4())
			orders := []q.RowOrder{{Name: "rank", Terms: []q.RowTerm{q.Desc(f.rank).NullsLast(), q.Asc(f.id), q.Asc(f.nid).NullsLast()}}, {Name: "reverse", Terms: []q.RowTerm{q.Asc(f.rank).NullsFirst(), q.Desc(f.id), q.Desc(f.nid).NullsFirst()}}}
			spec := q.ReadListSpec{Optional: []q.RowPredicate{{Name: "selected", When: q.Any(f.id.Expr(), ids.Expr())}, {Name: "ranked", When: q.Ge(f.rank.Expr(), minimum.Expr())}}, Orders: orders, Pages: q.RowPages{Unpaged: true, Offset: true, Keyset: true, MaxLimit: 10, MaxOffset: 20}, ExactCount: true}
			plan := q.DefineRead(f.out, "joined", f.query.WithList(spec))
			verifyListSnapshot(t, db, f)
			err := runRowPlan(t, db, pool, driver, f.reg.SQLAuthority(), f.role, 1, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				bound := plan.Bind(s)
				for _, order := range orders {
					all, total, err := bound.List(ctx, q.RowChoice{Order: order.Name})
					require.NoError(t, err)
					require.EqualValues(t, 6, total)
					require.Len(t, all, 6)
					choice := q.RowChoice{Order: order.Name, Page: q.FirstPage, Limit: 2}
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
						choice.Page, choice.After = q.AfterPage, cursor
					}
					require.Equal(t, all, combined)
					page, err := bound.Page(ctx, q.RowChoice{Order: order.Name, Page: q.OffsetPage, Limit: 2, Offset: 4})
					require.NoError(t, err)
					require.Equal(t, all[4:], page)
				}
				selected := q.BindValue(ids, []int64{1, 3})
				page, total, err := bound.List(ctx, q.RowChoice{Order: "rank", Filters: []string{"selected"}, Page: q.OffsetPage, Limit: 2}, selected)
				require.NoError(t, err)
				require.Len(t, page, 2)
				require.EqualValues(t, 3, total)
				total, err = bound.Count(ctx, []string{"ranked"}, q.BindValue(minimum, int32(0)))
				require.NoError(t, err)
				require.EqualValues(t, 2, total)
				for _, empty := range [][]int64{nil, {}} {
					page, total, err = bound.List(ctx, q.RowChoice{Order: "rank", Filters: []string{"selected"}}, q.BindValue(ids, empty))
					require.NoError(t, err)
					require.Empty(t, page)
					require.Zero(t, total)
				}
				for _, bad := range []q.RowChoice{{Order: "rank", Page: q.FirstPage, Limit: 11}, {Order: "rank", Page: q.OffsetPage, Limit: 1, Offset: 21}, {Order: "rank", Page: q.AfterPage, Limit: 1}, {Order: "rank", Filters: []string{"selected", "selected"}}, {Order: "missing"}} {
					_, err := bound.Page(ctx, bad, selected)
					require.Error(t, err)
				}
				return nil
			})
			require.NoError(t, err)
		})
	}
}
