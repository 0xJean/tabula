//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestWriteConflictGenericArbiterAndConditions(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			role := writeRole(t, db, schema)
			exec := func(sql string) { t.Helper(); _, err := db.ExecContext(ctx, sql); require.NoError(t, err) }
			exec("CREATE TABLE " + schema + `.items (id bigint PRIMARY KEY, rank int NOT NULL, label text NOT NULL);
CREATE UNIQUE INDEX items_rank ON ` + schema + `.items(rank) WHERE label=E'note''\\entry';
GRANT INSERT(id,rank,label), UPDATE(rank), SELECT(id,rank,label) ON ` + schema + ".items TO " + role)
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			table := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".items", RelationScope: q.Global()})
			src := q.MapTable(table).As("i")
			id, rank, label := q.FieldOf(src.Rel(), "id", q.Int8()), q.FieldOf(src.Rel(), "rank", q.Int4()), q.FieldOf(src.Rel(), "label", q.Text())
			key, value, text := q.Input("id", q.Int8()), q.Input("rank", q.Int4()), q.Input("label", q.Text())
			base := q.InsertInto(src).Values(q.Assign(id, key.Expr()), q.Assign(rank, value.Expr()), q.Assign(label, text.Expr()))
			partial := q.DefineWrite(src, "partial", base.OnConflict(q.ConflictColumns(rank.Column()).WhereIndex(q.IndexEq(label, "note'\\entry"))).DoNothing())
			def := table.Definition()
			statement := def.Statements["partial.global.write"]
			conn, err := db.Conn(ctx)
			require.NoError(t, err)
			defer conn.Close()
			defer func() {
				_, _ = conn.ExecContext(context.WithoutCancel(ctx), "RESET ROLE; RESET plan_cache_mode; DEALLOCATE ALL")
			}()
			_, err = conn.ExecContext(ctx, "SET plan_cache_mode=force_generic_plan; SET ROLE "+role+"; PREPARE items_plan AS "+statement)
			require.NoError(t, err)
			var raw []byte
			require.NoError(t, conn.QueryRowContext(ctx, `EXPLAIN (FORMAT JSON) EXECUTE items_plan(1,4,E'note''\\entry')`).Scan(&raw))
			var explain []struct {
				Plan struct {
					Indexes []string `json:"Conflict Arbiter Indexes"`
				} `json:"Plan"`
			}
			require.NoError(t, json.Unmarshal(raw, &explain))
			require.Equal(t, []string{"items_rank"}, explain[0].Plan.Indexes)
			var unique, immediate, valid bool
			require.NoError(t, conn.QueryRowContext(ctx, `SELECT indisunique,indimmediate,indisvalid FROM pg_catalog.pg_index WHERE indexrelid=$1::regclass`, schema+".items_rank").Scan(&unique, &immediate, &valid))
			require.True(t, unique && immediate && valid)
			_, err = conn.ExecContext(ctx, "DEALLOCATE items_plan; RESET ROLE; RESET plan_cache_mode")
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			run := authorizedEffectRunner(t, driver, db, pool, role, reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			bindings := func(id int64, rank int32, label string) []q.Binding {
				return []q.Binding{q.BindValue(key, id), q.BindValue(value, rank), q.BindValue(text, label)}
			}
			invoke := func(plan *q.WritePlan[struct{}], values ...q.Binding) (int64, error) {
				var count int64
				err := run(ctx, func(ctx context.Context, s session) error {
					var err error
					count, err = plan.Bind(s).Exec(ctx, values...)
					return err
				})
				return count, err
			}
			n, err := invoke(partial, bindings(1, 4, "note'\\entry")...)
			require.NoError(t, err)
			require.EqualValues(t, 1, n)
			n, err = invoke(partial, bindings(2, 4, "note'\\entry")...)
			require.NoError(t, err)
			require.Zero(t, n)
			exec("REVOKE SELECT(label) ON " + schema + ".items FROM " + role)
			_, err = invoke(partial, bindings(3, 5, "note'\\entry")...)
			require.Equal(t, "42501", q.SQLState(err))
			exec("GRANT SELECT(label) ON " + schema + ".items TO " + role)
			update := base.OnConflict(q.ConflictConstraint("items_pkey")).DoUpdate(q.Assign(rank, q.Excluded(rank))).UpdateWhere(q.Lt(rank.Expr(), q.Excluded(rank)))
			returned := q.DefineTableReturning(src, "upsert", update, q.SelectColumns(id.Column(), rank.Column()))
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				rows, err := returned.Bind(s).All(ctx, bindings(1, 3, "note")...)
				require.NoError(t, err)
				require.Empty(t, rows)
				rows, err = returned.Bind(s).All(ctx, bindings(1, 8, "note")...)
				require.NoError(t, err)
				require.Equal(t, []writeItem{{ID: 1, Rank: 8}}, rows)
				return nil
			}))
			duplicate := q.DefineWrite(src, "duplicate", base.Values(q.Assign(id, key.Expr()), q.Assign(rank, value.Expr()), q.Assign(label, text.Expr())).
				OnConflict(q.ConflictColumns(id.Column())).DoUpdate(q.Assign(rank, q.Excluded(rank))))
			_, err = invoke(duplicate, bindings(1, 9, "note")...)
			require.Equal(t, "21000", q.SQLState(err))
			var current int32
			require.NoError(t, db.QueryRowContext(ctx, "SELECT rank FROM "+schema+".items WHERE id=1").Scan(&current))
			require.EqualValues(t, 8, current)
			require.False(t, strings.Contains(statement, "note'\\entry")) // schema text is escaped
		})
	}
}

func TestWriteNamedExclusionDoNothing(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			_, err := db.ExecContext(ctx, "CREATE TABLE "+schema+`.items (id bigint PRIMARY KEY, rank int NOT NULL, label text DEFAULT 'note', CONSTRAINT items_rank EXCLUDE USING gist (int4range(rank,rank,'[]') WITH &&))`)
			require.NoError(t, err)
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			table := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".items", RelationScope: q.Global()})
			src := q.MapTable(table).As("i")
			id, rank := q.FieldOf(src.Rel(), "id", q.Int8()), q.FieldOf(src.Rel(), "rank", q.Int4())
			key, value := q.Input("id", q.Int8()), q.Input("rank", q.Int4())
			plan := q.DefineWrite(src, "append", q.InsertInto(src).Values(q.Assign(id, key.Expr()), q.Assign(rank, value.Expr())).OnConflict(q.ConflictConstraint("items_rank")).DoNothing())
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				n, err := plan.Bind(s).Exec(ctx, q.BindValue(key, int64(1)), q.BindValue(value, int32(5)))
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				n, err = plan.Bind(s).Exec(ctx, q.BindValue(key, int64(2)), q.BindValue(value, int32(5)))
				require.NoError(t, err)
				require.Zero(t, n)
				return nil
			}))
		})
	}
}
