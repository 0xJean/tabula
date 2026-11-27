//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	tplan "github.com/0xJean/tabula/plan"
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
			src, other := tplan.MapTable(table).As("_tabula_0"), tplan.MapTable(notes).As("_tabula_1")
			id, rank := tplan.FieldOf(src.Rel(), "id", tplan.Int8()), tplan.FieldOf(src.Rel(), "rank", tplan.Int4())
			nid, nrank := tplan.FieldOf(other.Rel(), "id", tplan.Int8()), tplan.FieldOf(other.Rel(), "rank", tplan.Int4())
			matched := tplan.DefineWrite(src, "matched", tplan.UpdateOf(src).Set(tplan.Assign(rank, nrank.Expr())).From(other, tplan.Eq(nid.Expr(), id.Expr())).AllRows())
			inner := tplan.SelectValue(nrank.Expr()).From(other).Where(tplan.Eq(nid.Expr(), tplan.Outer(id.Expr())))
			correlated := tplan.DefineWrite(src, "correlated", tplan.UpdateOf(src).Set(tplan.Assign(rank, tplan.Coalesce(tplan.ScalarSubquery(inner), rank.Expr()))).AllRows())
			remove := tplan.DefineWrite(src, "remove", tplan.DeleteFrom(src).Using(other, tplan.Eq(nid.Expr(), id.Expr())).AllRows())
			excluded := tplan.MapTable(table).As("excluded")
			eid, erank := tplan.FieldOf(excluded.Rel(), "id", tplan.Int8()), tplan.FieldOf(excluded.Rel(), "rank", tplan.Int4())
			key, value := tplan.Input("id", tplan.Int8()), tplan.Input("rank", tplan.Int4())
			upsert := tplan.DefineWrite(excluded, "upsert", tplan.InsertInto(excluded).Values(tplan.Assign(eid, key.Expr()), tplan.Assign(erank, value.Expr())).
				OnConflict(tplan.ConflictColumns(eid.Column())).DoUpdate(tplan.Assign(erank, tplan.Excluded(erank))))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			for _, plan := range []*tplan.WritePlan[struct{}]{matched, correlated} {
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
				n, err := upsert.Bind(s).Exec(ctx, tplan.BindValue(key, int64(1)), tplan.BindValue(value, int32(99)))
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
