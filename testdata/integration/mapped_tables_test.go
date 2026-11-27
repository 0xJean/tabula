//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/pgxdriver"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

type mappedItem struct {
	ID        int64     `db:"id,key"`
	GroupID   string    `db:"group_id"`
	Label     string    `db:"label"`
	Rank      int32     `db:"rank"`
	Active    bool      `db:"active"`
	Note      *string   `db:"note,secret"`
	Region    string    `db:"region"`
	Revision  int64     `db:"revision"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

func TestMappedTableDrivers(t *testing.T) {
	db, pool := databases(t)
	for _, driverName := range []string{"stdsql", "pgx"} {
		t.Run(driverName, func(t *testing.T) {
			schema := fmt.Sprintf("mapped_rows_%d", time.Now().UnixNano())
			reader, fullReader, writer := schema+"_reader", schema+"_full", schema+"_writer"
			tableName := schema + ".items"
			exec := func(query string) {
				t.Helper()
				_, err := db.ExecContext(t.Context(), query)
				require.NoError(t, err)
			}
			for _, role := range []string{reader, fullReader, writer} {
				exec("CREATE ROLE " + role + " NOBYPASSRLS")
				t.Cleanup(func() { _, err := db.Exec("DROP ROLE " + role); require.NoError(t, err) })
			}
			exec("CREATE SCHEMA " + schema)
			t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + schema + " CASCADE"); require.NoError(t, err) })
			exec("GRANT USAGE ON SCHEMA " + schema + " TO " + strings.Join([]string{reader, fullReader, writer}, ","))
			exec("CREATE TABLE " + tableName + ` (
				id bigint PRIMARY KEY, group_id uuid NOT NULL, label varchar(8) NOT NULL,
				rank integer NOT NULL, active boolean NOT NULL, note text, region text NOT NULL,
				revision bigint NOT NULL, created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL)`)
			const groupID = "00000000-0000-0000-0000-000000000001"
			exec("INSERT INTO " + tableName + " VALUES (1,'" + groupID + "','first',4,true,'private','1',8,'2020-03-02 01:00+00','2020-03-02 02:00+00')," +
				"(2,'" + groupID + "','other',5,false,NULL,'2',9,'2020-03-03 01:00+00','2020-03-03 02:00+00')")
			exec("ALTER TABLE " + tableName + " ENABLE ROW LEVEL SECURITY")
			exec("ALTER TABLE " + tableName + " FORCE ROW LEVEL SECURITY")
			exec("CREATE POLICY visible ON " + tableName + " USING (region = current_setting('demo.scope',true))")
			exec("GRANT SELECT(id,group_id,label) ON " + tableName + " TO " + reader)
			exec("GRANT SELECT ON " + tableName + " TO " + fullReader)
			exec("GRANT SELECT(group_id), UPDATE(rank), DELETE ON " + tableName + " TO " + writer)

			reg := q.NewRegistry(q.RegistryConfig[itemScope]{Deny: q.DenyLegacySQL})
			run := func(role string, f rowCallback) error {
				return runRowPlan(t, db, pool, driverName, reg.SQLAuthority(), role, 1, q.LogConfig{}, f)
			}
			tab := q.DefinePlanSource(reg, q.TableSpec[mappedItem, int64]{Name: tableName, AllowDelete: true, RelationScope: q.ByRLS()})
			layout := tplan.MapTable(tab, tplan.ColumnType("group_id", pg.UUID[string]()), tplan.ColumnType("label", pg.Varchar(8)))
			src := layout.As("i")
			group := tplan.MappedColumn[string](layout, "group_id")
			v := group.Input("group_id")
			filter := tplan.RowFilter{Where: tplan.Eq(group.On(src.Rel()).Expr(), v.Expr())}
			value := tplan.BindValue(v, groupID)
			id, label := tplan.MappedColumn[int64](layout, "id"), tplan.MappedColumn[string](layout, "label")
			narrow := tplan.DefineTableRows(src, "brief", tplan.RowsSpec{RowFilter: filter},
				tplan.SelectColumns(label.On(src.Rel()).Column(), id.On(src.Rel()).Column()), tplan.ReadForms{Exact: true, First: tplan.UnorderedFirst})
			probe := tplan.DefineExists(src, "has_group", filter)
			out := q.DefineView(reg, q.ViewSpec[sourceResult]{Name: "entries"})
			rowID := tplan.Project(tplan.Output(out, "id", pg.Int8()), id.On(src.Rel()).Expr())
			rowLabel := func(r tplan.Rel[mappedItem]) tplan.Projection {
				return tplan.Project(tplan.Output(out, "label", pg.Varchar(8)), label.On(r).Expr())
			}
			projected := tplan.DefineRows(out, "brief", src, tplan.RowsSpec{RowFilter: filter}, rowID, rowLabel(src.Rel()))
			right := layout.As("j")
			joined := tplan.DefineRead(out, "paired", tplan.Select(rowID, rowLabel(right.Rel())).From(src).
				InnerJoin(right.Rel(), tplan.Eq(id.On(src.Rel()).Expr(), id.On(right.Rel()).Expr())).Where(filter.Where))
			checkSourceRole(t, db, reader, tab.Definition())
			checkSourceRole(t, db, reader, q.Definition(out.Definition()))
			require.NoError(t, run(reader, func(ctx context.Context, s q.Session[itemScope]) error {
				bound := narrow.Bind(s)
				got, n, err := bound.List(ctx, tplan.RowChoice{}, value)
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				require.Equal(t, []mappedItem{{ID: 1, Label: "first"}}, got)
				one, err := bound.One(ctx, tplan.RowChoice{}, value)
				require.NoError(t, err)
				require.Equal(t, got[0], one)
				first, err := bound.First(ctx, tplan.RowChoice{}, value)
				require.NoError(t, err)
				require.Equal(t, got[0], first)
				maybe, ok, err := bound.MaybeOne(ctx, tplan.RowChoice{}, tplan.BindValue(v, "00000000-0000-0000-0000-000000000000"))
				require.NoError(t, err)
				require.False(t, ok)
				require.Equal(t, mappedItem{}, maybe)
				exists, err := probe.Bind(s).Exists(ctx, nil, value)
				require.NoError(t, err)
				require.True(t, exists)
				want := []sourceResult{{ID: 1, Label: "first"}}
				views, err := projected.Bind(s).Find(ctx, tplan.RowChoice{}, value)
				require.NoError(t, err)
				require.Equal(t, want, views)
				views, err = joined.Bind(s).Find(ctx, value)
				require.NoError(t, err)
				require.Equal(t, want, views)
				return nil
			}))
			all := tplan.DefineTableRows(src, "by_group", tplan.RowsSpec{RowFilter: filter}, tplan.SelectAll())
			checkSourceRole(t, db, fullReader, tab.Definition())
			families := sqlcheck.DefaultTypeFamilies()
			families["string"] = append(families["string"], "uuid")
			drift, err := sqlcheck.VerifySchema(t.Context(), db, []q.Definition{tab.Definition()}, families)
			require.NoError(t, err)
			require.Empty(t, drift)
			require.NoError(t, run(fullReader, func(ctx context.Context, s q.Session[itemScope]) error {
				got, err := all.Bind(s).Find(ctx, tplan.RowChoice{}, value)
				require.NoError(t, err)
				require.Len(t, got, 1)
				got[0].CreatedAt, got[0].UpdatedAt = got[0].CreatedAt.UTC(), got[0].UpdatedAt.UTC()
				require.Equal(t, mappedItem{ID: 1, GroupID: groupID, Label: "first", Rank: 4, Active: true, Note: new("private"),
					Region: "1", Revision: 8, CreatedAt: time.Date(2020, 3, 2, 1, 0, 0, 0, time.UTC),
					UpdatedAt: time.Date(2020, 3, 2, 2, 0, 0, 0, time.UTC)}, got[0])
				rows, err := all.Bind(s).Find(ctx, tplan.RowChoice{}, tplan.BindValue(v, "00000000-0000-0000-0000-000000000000"))
				require.NoError(t, err)
				require.Nil(t, rows)
				return nil
			}))
			require.Equal(t, "42501", q.SQLState(run(reader, func(ctx context.Context, s q.Session[itemScope]) error {
				_, err := all.Bind(s).Find(ctx, tplan.RowChoice{}, value)
				return err
			})))
			rank := tplan.MappedColumn[int32](layout, "rank")
			newRank := rank.Input("rank")
			change := tplan.DefineUpdateMany(src, "change", tplan.MutationSpec{RowFilter: filter},
				tplan.RowSet{Name: "rank", Assignments: []tplan.RowAssignment{tplan.Assign(rank.On(src.Rel()), newRank.Expr())}})
			remove := tplan.DefineDeleteMany(src, "remove", tplan.MutationSpec{RowFilter: filter})
			require.NoError(t, run(writer, func(ctx context.Context, s q.Session[itemScope]) error {
				n, err := change.Bind(s).UpdateMany(ctx, "rank", nil, value, tplan.BindValue(newRank, 12))
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				n, err = remove.Bind(s).DeleteMany(ctx, nil, value)
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				return nil
			}))
			var remaining int
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+tableName).Scan(&remaining))
			require.Equal(t, 1, remaining)

			exec("ALTER TABLE " + tableName + " ALTER COLUMN label TYPE varchar(16)")
			work := sqlcheck.RoleStatements{}
			work.AddViews(reader, out.Definition())
			failures, err := sqlcheck.VerifyRoleStatements(t.Context(), db, work)
			require.NoError(t, err)
			require.NotEmpty(t, failures)
			for _, failure := range failures {
				require.Contains(t, failure, "modifier")
			}
			exec("ALTER TABLE " + tableName + " ALTER COLUMN rank DROP NOT NULL")
			drift, err = sqlcheck.VerifySchema(t.Context(), db, []q.Definition{tab.Definition()}, families)
			require.NoError(t, err)
			require.Len(t, drift, 1)
			require.Contains(t, drift[0], ".rank: schema is nullable")
			checkMappedKeyLock(t, db, schema, writer, reg, exec, run)
			checkMappedCodecs(t, db, schema, fullReader, driverName, exec, func(auth q.SQLAuthority, f rowCallback) error {
				return runRowPlan(t, db, pool, driverName, auth, fullReader, 1, q.LogConfig{}, f)
			})
		})
	}
}

func checkMappedKeyLock(t *testing.T, db *sql.DB, schema, role string, reg *q.Registry[itemScope], exec func(string), run func(string, rowCallback) error) {
	t.Helper()
	name := schema + ".groups"
	exec("CREATE TABLE " + name + " (group_id bigint, item_id bigint, PRIMARY KEY (group_id,item_id))")
	exec("INSERT INTO " + name + " VALUES (4,7)")
	exec("GRANT SELECT (item_id), UPDATE (group_id) ON " + name + " TO " + role)
	tab := q.DefinePlanSource(reg, q.TableSpec[sourceKey, sourceKey]{Name: name, RelationScope: q.Global()})
	layout := tplan.MapTable(tab)
	src := layout.As("g")
	item := tplan.MappedColumn[int64](layout, "item_id").On(src.Rel())
	plan := tplan.DefineTableRows(src, "hold", tplan.RowsSpec{Lock: tplan.ForRows(tplan.ForUpdate, src.Rel(), tplan.NoWait)},
		tplan.SelectColumns(item.Column()), tplan.ReadForms{First: tplan.UnorderedFirst})
	checkSourceRole(t, db, role, tab.Definition())
	hold := func(ctx context.Context, s q.Session[itemScope]) error {
		row, err := plan.Bind(s).First(ctx, tplan.RowChoice{})
		if err != nil {
			return err
		}
		require.Equal(t, sourceKey{ItemID: 7}, row)
		_, conflict := db.ExecContext(ctx, "SELECT item_id FROM "+name+" FOR UPDATE NOWAIT")
		require.Equal(t, "55P03", q.SQLState(conflict))
		return nil
	}
	require.NoError(t, run(role, hold))
	exec("REVOKE UPDATE (group_id) ON " + name + " FROM " + role)
	checkSourceRole(t, db, role, tab.Definition(), sqlcheck.MissingRequirement{
		Kind: "TABLE", Object: `"` + schema + `"."groups"`, Privilege: "UPDATE ANY COLUMN"})
	require.Equal(t, "42501", q.SQLState(run(role, hold)))
}

type mappedMark string

func (m *mappedMark) Scan(v any) error {
	var s sql.NullString
	if err := s.Scan(v); err != nil {
		return err
	}
	*m = mappedMark(strings.ToLower(s.String))
	return nil
}

func (m mappedMark) Value() (driver.Value, error) { return string(m), nil }

type mappedNote struct {
	ID       int64          `db:"id,key"`
	Mark     mappedMark     `db:"mark"`
	Optional *mappedMark    `db:"optional"`
	Title    string         `db:"title,zero"`
	Detail   map[string]int `db:"detail,json"`
	Labels   []string       `db:"labels,array"`
	State    mappedMark     `db:"state,deleted"`
}

func checkMappedCodecs(t *testing.T, db *sql.DB, schema, role, driverName string, exec func(string), run func(q.SQLAuthority, rowCallback) error) {
	t.Helper()
	table := schema + ".notes"
	markType := schema + ".mark"
	exec("CREATE TYPE " + markType + " AS ENUM('OPEN','CLOSED','FIRST')")
	exec("CREATE TABLE " + table + " (id bigint PRIMARY KEY, mark " + markType + " NOT NULL, optional " + markType +
		", title text, detail jsonb NOT NULL, labels text[], state " + markType + " NOT NULL)")
	exec("INSERT INTO " + table + ` VALUES
		(1,'FIRST',NULL,NULL,'{"n":1}',ARRAY['blue','red'],'OPEN'),
		(2,'FIRST','OPEN','title','{}','{}','OPEN'),
		(3,'FIRST',NULL,NULL,'{}',NULL,'CLOSED')`)
	exec("GRANT SELECT ON " + table + " TO " + role)
	calls := 0
	kind := q.CatalogType[mappedMark](schema, "mark", nil).WithEncoder(func(v mappedMark) (any, error) {
		calls++
		return strings.ToUpper(string(v)), nil
	})
	cfg := q.RegistryConfig[itemScope]{Deny: q.DenyLegacySQL, DefaultTypes: []q.TypeDefault{q.TypeMapping(kind),
		q.TypeMapping(pg.JSONB[map[string]int]()), q.TypeMapping(pg.ArrayOf(pg.Text()))}, ArrayColumn: pgxdriver.ArrayColumn}
	if driverName == "stdsql" {
		cfg.ArrayArg, cfg.ArrayColumn = pqdriver.Array, pqdriver.ArrayColumn
	}
	reg := q.NewRegistry(cfg)
	tab := q.DefinePlanSource(reg, q.TableSpec[mappedNote, int64]{Name: table, RelationScope: q.Global(),
		SoftDelete: &q.SoftDelete{AliveValue: mappedMark("open"), DeletedValue: mappedMark("closed")}})
	layout := tplan.MapTable(tab)
	src, other := layout.As("n"), layout.As("m")
	id := tplan.MappedColumn[int64](layout, "id")
	mark := tplan.MappedColumn[mappedMark](layout, "mark")
	input := mark.Input("mark")
	filter := tplan.RowFilter{Where: tplan.Eq(mark.On(src.Rel()).Expr(), input.Expr())}
	full := tplan.DefineTableRows(src, "notes", tplan.RowsSpec{RowFilter: filter, Orders: []tplan.RowOrder{{Name: "id", Terms: []tplan.RowTerm{tplan.Asc(id.On(src.Rel()))}}}}, tplan.SelectAll())
	type codeRow struct {
		Mark mappedMark `db:"mark"`
	}
	out := q.DefineView(reg, q.ViewSpec[codeRow]{Name: "entries"})
	joined := tplan.DefineRead(out, "pairs", tplan.Select(tplan.Project(tplan.Output(out, "mark", kind), mark.On(other.Rel()).Expr())).
		From(src.Rel()).InnerJoin(other, tplan.Eq(id.On(src.Rel()).Expr(), id.On(other.Rel()).Expr())).Where(filter.Where))
	checkSourceRole(t, db, role, tab.Definition())
	checkSourceRole(t, db, role, q.Definition(out.Definition()))
	require.Equal(t, 1, calls) // The declared live value is shared by the layout's occurrences.
	b := tplan.BindValue(input, "first")
	require.NoError(t, run(reg.SQLAuthority(), func(ctx context.Context, s q.Session[itemScope]) error {
		rows, n, err := full.Bind(s).List(ctx, tplan.RowChoice{Order: "id"}, b)
		require.NoError(t, err)
		require.EqualValues(t, 2, n)
		require.Equal(t, []mappedNote{
			{ID: 1, Mark: "first", Detail: map[string]int{"n": 1}, Labels: []string{"blue", "red"}, State: "open"},
			{ID: 2, Mark: "first", Optional: new(mappedMark("open")), Title: "title", Detail: map[string]int{}, Labels: []string{}, State: "open"},
		}, rows)
		got, err := joined.Bind(s).Find(ctx, b)
		require.NoError(t, err)
		require.Equal(t, []codeRow{{"first"}, {"first"}}, got)
		return nil
	}))
	require.Equal(t, 2, calls)
}
