//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestWriteScopeVersionAndLiveRows(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			role := writeRole(t, db, schema)
			_, err := db.ExecContext(ctx, "CREATE TABLE "+schema+`.items(id bigint PRIMARY KEY, scope_id bigint NOT NULL, rank int NOT NULL DEFAULT 0,
label text, revision int NOT NULL DEFAULT 0, removed bool NOT NULL DEFAULT false);
CREATE TABLE `+schema+`.notes(id bigint PRIMARY KEY, rank int NOT NULL, label text NOT NULL);
INSERT INTO `+schema+`.notes VALUES(1,2,'first'),(2,3,'second');
GRANT INSERT(id,scope_id,rank),SELECT(id,scope_id,revision,removed),UPDATE(rank,revision),DELETE ON `+schema+`.items TO `+role+`;
GRANT SELECT(id,rank) ON `+schema+`.notes TO `+role)
			require.NoError(t, err)
			reg := q.NewRegistry(q.RegistryConfig[itemScope]{Deny: q.DenyLegacySQL, ScopeID: func(s itemScope) any { return int64(s) }})
			table := q.DefinePlanSource(reg, q.TableSpec[planItem, int64]{Name: schema + ".items", AllowDelete: true})
			src := q.MapTable(table, q.ColumnType("scope_id", q.CatalogType[itemScope]("pg_catalog", "int8", nil))).As("i")
			notes := q.MapTable(q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".notes", RelationScope: q.Global()})).As("n")
			id, rank, revision := q.FieldOf(src.Rel(), "id", q.Int8()), q.FieldOf(src.Rel(), "rank", q.Int4()), q.FieldOf(src.Rel(), "revision", q.Int4())
			nid, nrank := q.FieldOf(notes.Rel(), "id", q.Int8()), q.FieldOf(notes.Rel(), "rank", q.Int4())
			key, value, previous := q.Input("id", q.Int8()), q.Input("rank", q.Int4()), q.Input("previous", q.Int4())
			copyRows := q.DefineWrite(src, "copy", q.InsertInto(src).Columns(id.Column(), rank.Column()).FromSelect(
				q.Select(q.Export("id", nid.Expr()).Project(), q.Export("rank", nrank.Expr()).Project()).From(notes)))
			insert := q.DefineWrite(src, "append", q.InsertInto(src).Values(q.Assign(id, key.Expr()), q.Assign(rank, value.Expr())))
			update := q.DefineWrite(src, "change", q.UpdateOf(src).Set(q.Assign(rank, value.Expr())).AllRows().Version(q.ExpectVersion(revision, previous)))
			remove := q.DefineWrite(src, "remove", q.DeleteFrom(src).AllRows().Version(q.ExpectVersion(revision, previous)))
			run := func(scope itemScope, body rowCallback) error {
				return runRowPlan(t, db, pool, driver, reg.SQLAuthority(), role, scope, q.LogConfig{}, body)
			}
			require.NoError(t, run(1, func(ctx context.Context, s q.Session[itemScope]) error {
				n, err := copyRows.Bind(s).Exec(ctx)
				require.NoError(t, err)
				require.EqualValues(t, 2, n)
				return nil
			}))
			require.NoError(t, run(2, func(ctx context.Context, s q.Session[itemScope]) error {
				_, err := insert.Bind(s).Exec(ctx, q.BindValue(key, int64(3)), q.BindValue(value, int32(4)))
				return err
			}))
			_, err = db.ExecContext(ctx, "UPDATE "+schema+".items SET removed=true WHERE id=2")
			require.NoError(t, err)
			require.NoError(t, run(1, func(ctx context.Context, s q.Session[itemScope]) error {
				n, err := update.Bind(s).Exec(ctx, q.BindValue(value, int32(9)), q.BindValue(previous, int32(0)))
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				n, err = update.Bind(s).Exec(ctx, q.BindValue(value, int32(10)), q.BindValue(previous, int32(0)))
				require.NoError(t, err)
				require.Zero(t, n)
				n, err = remove.Bind(s).Exec(ctx, q.BindValue(previous, int32(1)))
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				return nil
			}))
			var count, total int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*),sum(rank) FROM "+schema+".items").Scan(&count, &total))
			require.Equal(t, 2, count)
			require.Equal(t, 7, total)
			for _, key := range []string{"copy.scoped.write", "append.scoped.write"} {
				shape := table.Definition().StatementShapes[key]
				require.True(t, shape.Inputs[0].Scope)
				for _, access := range shape.Access {
					if access.Relation == `"`+schema+`"."items"` {
						require.Equal(t, "INSERT", access.Command)
					}
				}
			}
		})
	}
}

func TestWriteRLSScopeDefaults(t *testing.T) {
	type item struct {
		ID    int64     `db:"id,key"`
		Scope itemScope `db:"scope_id,scope"`
		Rank  int32     `db:"rank"`
	}
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			role := writeRole(t, db, schema)
			_, err := db.ExecContext(ctx, "CREATE TABLE "+schema+`.items(id bigint PRIMARY KEY, scope_id bigint NOT NULL DEFAULT nullif(current_setting('demo.scope',true),'')::bigint, rank int NOT NULL);
ALTER TABLE `+schema+`.items ENABLE ROW LEVEL SECURITY; ALTER TABLE `+schema+`.items FORCE ROW LEVEL SECURITY;
CREATE POLICY visible ON `+schema+`.items TO `+role+` USING(scope_id=nullif(current_setting('demo.scope',true),'')::bigint) WITH CHECK(scope_id=nullif(current_setting('demo.scope',true),'')::bigint);
GRANT INSERT(id,rank),UPDATE(rank) ON `+schema+`.items TO `+role)
			require.NoError(t, err)
			reg := q.NewRegistry(q.RegistryConfig[itemScope]{Deny: q.DenyLegacySQL})
			table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: schema + ".items", RelationScope: q.ByRLS()})
			src := q.MapTable(table).As("i")
			id, rank := q.FieldOf(src.Rel(), "id", q.Int8()), q.FieldOf(src.Rel(), "rank", q.Int4())
			key, value := q.Input("id", q.Int8()), q.Input("rank", q.Int4())
			plan := q.DefineWrite(src, "append", q.InsertInto(src).Columns(id.Column(), rank.Column()).FromSelect(
				q.Select(q.Export("id", key.Expr()).Project(), q.Export("rank", value.Expr()).Project())))
			update := q.DefineWrite(src, "change", q.UpdateOf(src).Set(q.Assign(rank, value.Expr())).AllRows())
			for _, shape := range table.Definition().StatementShapes {
				require.Len(t, shape.ForceRLS, 1)
				for _, input := range shape.Inputs {
					require.False(t, input.Scope)
				}
			}
			for _, scope := range []itemScope{1, 2} {
				require.NoError(t, runRowPlan(t, db, pool, driver, reg.SQLAuthority(), role, scope, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
					_, err := plan.Bind(s).Exec(ctx, q.BindValue(key, int64(scope)), q.BindValue(value, int32(3)))
					return err
				}))
			}
			require.NoError(t, runRowPlan(t, db, pool, driver, reg.SQLAuthority(), role, 1, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				n, err := update.Bind(s).Exec(ctx, q.BindValue(value, int32(8)))
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				return nil
			}))
			var value1, value2 int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT rank FROM "+schema+".items WHERE scope_id=1").Scan(&value1))
			require.NoError(t, db.QueryRowContext(ctx, "SELECT rank FROM "+schema+".items WHERE scope_id=2").Scan(&value2))
			require.Equal(t, 8, value1)
			require.Equal(t, 3, value2)
		})
	}
}
