//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

type sourceItem struct {
	ID    int64  `db:"id,key,serial"`
	Label string `db:"label"`
	Rank  int32  `db:"rank"`
	Note  string `db:"note"`
}

type sourceResult struct {
	ID    int64  `db:"id"`
	Label string `db:"label"`
}

type sourceKey struct {
	GroupID int64 `db:"group_id,key"`
	ItemID  int64 `db:"item_id,key"`
}

func checkSourceRole(t *testing.T, db *sql.DB, role string, def q.Definition, want ...sqlcheck.MissingRequirement) {
	t.Helper()
	work := sqlcheck.RoleStatements{}
	work.AddDefinitions(role, def)
	require.NotEmpty(t, work[role])
	for _, s := range work[role] {
		require.NotNil(t, s.Shape, s.Name)
	}
	needs, err := work.Readiness(role)
	require.NoError(t, err)
	got, err := sqlcheck.CheckReadiness(t.Context(), db, role, needs)
	require.NoError(t, err)
	require.ElementsMatch(t, want, got)
	failures, err := sqlcheck.VerifyRoleStatements(t.Context(), db, work)
	require.NoError(t, err)
	if len(want) == 0 {
		require.Empty(t, failures)
		return
	}
	require.NotEmpty(t, failures)
	for _, message := range failures {
		require.Contains(t, message, "42501")
	}
}

func TestPlanSourceDrivers(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			schema := fmt.Sprintf("plan_source_%d", time.Now().UnixNano())
			reader, writer := schema+"_reader", schema+"_writer"
			exec := func(query string) {
				t.Helper()
				_, err := db.ExecContext(t.Context(), query)
				require.NoError(t, err)
			}
			for _, role := range []string{reader, writer} {
				exec("CREATE ROLE " + role + " NOBYPASSRLS")
				t.Cleanup(func() { _, err := db.Exec("DROP ROLE " + role); require.NoError(t, err) })
			}
			exec("CREATE SCHEMA " + schema)
			t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + schema + " CASCADE"); require.NoError(t, err) })
			exec("GRANT USAGE ON SCHEMA " + schema + " TO " + reader + ", " + writer)
			reg := q.NewRegistry(q.RegistryConfig[itemScope]{Deny: q.DenyLegacySQL})
			run := func(role string, body rowCallback) error {
				return runRowPlan(t, db, pool, driver, reg.SQLAuthority(), role, 1, q.LogConfig{}, body)
			}

			items := schema + ".items"
			exec("CREATE TABLE " + items + " (id bigserial PRIMARY KEY, label text NOT NULL, rank integer NOT NULL, note text NOT NULL)")
			exec("INSERT INTO " + items + " (label,rank,note) VALUES ('first',1,'')")
			exec("ALTER TABLE " + items + " ENABLE ROW LEVEL SECURITY")
			exec("ALTER TABLE " + items + " FORCE ROW LEVEL SECURITY")
			exec("CREATE POLICY visible ON " + items + " USING (true) WITH CHECK (true)")
			exec("GRANT SELECT (id,label) ON " + items + " TO " + reader)
			exec("GRANT SELECT (id), UPDATE (rank) ON " + items + " TO " + writer)

			table := q.DefinePlanSource(reg, q.TableSpec[sourceItem, int64]{Name: items, RelationScope: q.ByRLS(),
				WriteMode: q.WriteUpdateOnly, UpdateColumns: []string{"rank"}})
			i := q.FromTable(table, "i", q.ByRLS())
			src := q.RowsFrom(table, i)
			id, label, rank := q.FieldOf(i, "id", q.Int8()), q.FieldOf(i, "label", q.Text()), q.FieldOf(i, "rank", q.Int4())
			key, value := q.Input("key", q.Int8()), q.Input("value", q.Int4())
			where := q.RowFilter{Where: q.Eq(id.Expr(), key.Expr())}
			out := q.DefineView(reg, q.ViewSpec[sourceResult]{Name: "entries"})
			projection := []q.Projection{q.Project(q.Output(out, "id", q.Int8()), id.Expr()), q.Project(q.Output(out, "label", q.Text()), label.Expr())}
			search := q.DefineRows(out, "search", src, q.RowsSpec{RowFilter: where}, projection...)
			other := q.FromTable(table, "j", q.ByRLS())
			lookup := q.DefineRead(out, "lookup", q.Select(projection[0],
				q.Project(q.Output(out, "label", q.Text()), q.FieldOf(other, "label", q.Text()).Expr())).
				From(src).InnerJoin(other, q.Eq(id.Expr(), q.FieldOf(other, "id", q.Int8()).Expr())).Where(where.Where))
			change := q.DefineUpdateMany(src, "change", q.MutationSpec{RowFilter: where}, q.RowSet{
				Name: "rank", Assignments: []q.RowAssignment{q.Assign(rank, value.Expr())},
			})
			read, write := q.Definition(out.Definition()), table.Definition()
			needs, empty := sqlcheck.CollectPrivilegeNeeds([]q.Definition{write, read})
			require.Empty(t, empty)
			require.Equal(t, map[string]map[string]map[string]bool{items: {"SELECT": {"id": true, "label": true}, "UPDATE": {"rank": true}}}, needs.Columns)
			require.Empty(t, needs.Sequences)
			checkSourceRole(t, db, reader, read)
			checkSourceRole(t, db, writer, write)
			require.NoError(t, run(reader, func(ctx context.Context, s q.Session[itemScope]) error {
				got, err := search.Bind(s).Find(ctx, q.RowChoice{}, q.BindValue(key, 1))
				require.NoError(t, err)
				require.Equal(t, []sourceResult{{1, "first"}}, got)
				joined, err := lookup.Bind(s).Find(ctx, q.BindValue(key, 1))
				require.NoError(t, err)
				require.Equal(t, got, joined)
				_, err = table.Bind(s).Get(ctx, 1)
				require.ErrorIs(t, err, q.ErrOperationDenied)
				return nil
			}))
			update := func(ctx context.Context, s q.Session[itemScope]) error {
				n, err := change.Bind(s).UpdateMany(ctx, "rank", nil, q.BindValue(key, 1), q.BindValue(value, 7))
				if err == nil {
					require.EqualValues(t, 1, n)
				}
				return err
			}
			require.NoError(t, run(writer, update))
			var saved int
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT rank FROM "+items+" WHERE id=1").Scan(&saved))
			require.Equal(t, 7, saved)

			obj := `"` + schema + `"."items"`
			exec("REVOKE SELECT (label) ON " + items + " FROM " + reader)
			checkSourceRole(t, db, reader, read, sqlcheck.MissingRequirement{Kind: "COLUMN", Object: obj, Column: "label", Privilege: "SELECT"})
			exec("REVOKE SELECT (id) ON " + items + " FROM " + writer)
			checkSourceRole(t, db, writer, write, sqlcheck.MissingRequirement{Kind: "COLUMN", Object: obj, Column: "id", Privilege: "SELECT"})
			require.Equal(t, "42501", q.SQLState(run(writer, update)))
			exec("GRANT SELECT (id) ON " + items + " TO " + writer)
			exec("REVOKE UPDATE (rank) ON " + items + " FROM " + writer)
			checkSourceRole(t, db, writer, write, sqlcheck.MissingRequirement{Kind: "COLUMN", Object: obj, Column: "rank", Privilege: "UPDATE"})
			require.Equal(t, "42501", q.SQLState(run(writer, update)))

			checkSourceKeyLock(t, db, schema, writer, exec, reg, run)
		})
	}
}

func checkSourceKeyLock(t *testing.T, db *sql.DB, schema, role string, exec func(string), reg *q.Registry[itemScope], run func(string, rowCallback) error) {
	t.Helper()
	name := schema + ".groups"
	exec("CREATE TABLE " + name + " (group_id bigint, item_id bigint, PRIMARY KEY (group_id,item_id))")
	exec("INSERT INTO " + name + " VALUES (3,5)")
	exec("GRANT SELECT (item_id), UPDATE (group_id) ON " + name + " TO " + role)
	table := q.DefinePlanSource(reg, q.TableSpec[sourceKey, sourceKey]{Name: name, RelationScope: q.Global()})
	g := q.FromTable(table, "g", q.Global())
	type itemID struct {
		ID int64 `db:"id"`
	}
	out := q.DefineView(reg, q.ViewSpec[itemID]{Name: "notes"})
	hold := q.DefineRows(out, "hold", q.RowsFrom(table, g), q.RowsSpec{Lock: q.ForRows(q.ForUpdate, g, q.NoWait)},
		q.Project(q.Output(out, "id", q.Int8()), q.FieldOf(g, "item_id", q.Int8()).Expr()))
	require.Empty(t, table.Definition().Statements)
	d := q.Definition(out.Definition())
	checkSourceRole(t, db, role, d)
	lock := func(ctx context.Context, s q.Session[itemScope]) error {
		rows, err := hold.Bind(s).Find(ctx, q.RowChoice{})
		if err == nil {
			require.Equal(t, []itemID{{5}}, rows)
			// A separate transaction encounters the lock before the callback commits.
			_, conflict := db.ExecContext(ctx, "SELECT item_id FROM "+name+" FOR UPDATE NOWAIT")
			require.Equal(t, "55P03", q.SQLState(conflict))
		}
		return err
	}
	require.NoError(t, run(role, lock))
	exec("REVOKE UPDATE (group_id) ON " + name + " FROM " + role)
	checkSourceRole(t, db, role, d, sqlcheck.MissingRequirement{Kind: "TABLE", Object: `"` + schema + `"."groups"`, Privilege: "UPDATE ANY COLUMN"})
	require.Equal(t, "42501", q.SQLState(run(role, lock)))
}
