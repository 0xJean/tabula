//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestWriteTargetAliasesCannotCaptureSources(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			_, err := db.ExecContext(ctx, "CREATE TABLE "+schema+`.items(id bigint PRIMARY KEY, rank int NOT NULL, label text NOT NULL DEFAULT 'note');
CREATE TABLE `+schema+`.notes(LIKE `+schema+`.items INCLUDING ALL);
INSERT INTO `+schema+`.items(id,rank) VALUES(1,1),(2,2);
INSERT INTO `+schema+`.notes(id,rank) VALUES(1,21),(2,22)`)
			require.NoError(t, err)
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			table := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".items", RelationScope: q.Global(), AllowDelete: true})
			notes := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".notes", RelationScope: q.Global()})
			src, other := q.MapTable(table).As("_tabula_0"), q.MapTable(notes).As("_tabula_1")
			id, rank := q.FieldOf(src.Rel(), "id", q.Int8()), q.FieldOf(src.Rel(), "rank", q.Int4())
			nid, nrank := q.FieldOf(other.Rel(), "id", q.Int8()), q.FieldOf(other.Rel(), "rank", q.Int4())
			matched := q.DefineWrite(src, "matched", q.UpdateOf(src).Set(q.Assign(rank, nrank.Expr())).From(other, q.Eq(nid.Expr(), id.Expr())).AllRows())
			inner := q.SelectValue(nrank.Expr()).From(other).Where(q.Eq(nid.Expr(), q.Outer(id.Expr())))
			correlated := q.DefineWrite(src, "correlated", q.UpdateOf(src).Set(q.Assign(rank, q.Coalesce(q.ScalarSubquery(inner), rank.Expr()))).AllRows())
			remove := q.DefineWrite(src, "remove", q.DeleteFrom(src).Using(other, q.Eq(nid.Expr(), id.Expr())).AllRows())
			excluded := q.MapTable(table).As("excluded")
			eid, erank := q.FieldOf(excluded.Rel(), "id", q.Int8()), q.FieldOf(excluded.Rel(), "rank", q.Int4())
			key, value := q.Input("id", q.Int8()), q.Input("rank", q.Int4())
			upsert := q.DefineWrite(excluded, "upsert", q.InsertInto(excluded).Values(q.Assign(eid, key.Expr()), q.Assign(erank, value.Expr())).
				OnConflict(q.ConflictColumns(eid.Column())).DoUpdate(q.Assign(erank, q.Excluded(erank))))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			for _, plan := range []*q.WritePlan[struct{}]{matched, correlated} {
				require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
					n, err := plan.Bind(s).Exec(ctx)
					require.NoError(t, err)
					require.EqualValues(t, 2, n)
					return nil
				}))
			}
			var total int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT sum(rank) FROM "+schema+".items").Scan(&total))
			require.Equal(t, 43, total)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				n, err := upsert.Bind(s).Exec(ctx, q.BindValue(key, int64(1)), q.BindValue(value, int32(99)))
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				return nil
			}))
			require.NoError(t, db.QueryRowContext(ctx, "SELECT sum(rank) FROM "+schema+".items").Scan(&total))
			require.Equal(t, 121, total)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				n, err := remove.Bind(s).Exec(ctx)
				require.NoError(t, err)
				require.EqualValues(t, 2, n)
				return nil
			}))
		})
	}
}
