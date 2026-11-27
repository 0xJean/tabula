//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/stretchr/testify/require"
)

func TestWriteCommonTablesAndEffectOnly(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			_, err := db.ExecContext(ctx, "CREATE TABLE "+schema+`.items(id bigint PRIMARY KEY, rank int NOT NULL, label text NOT NULL DEFAULT 'note');
CREATE TABLE `+schema+`.entries(LIKE `+schema+`.items INCLUDING ALL);
CREATE TABLE `+schema+`.jobs(LIKE `+schema+`.items INCLUDING ALL);
INSERT INTO `+schema+`.jobs VALUES(1,0,'first'),(2,0,'second');
INSERT INTO `+schema+`.items VALUES(1,2,'first'),(2,3,'second')`)
			require.NoError(t, err)
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			table := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".items", RelationScope: q.Global()})
			entries := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".entries", RelationScope: q.Global(), WriteMode: q.WriteInsertOnly})
			i, e := q.MapTable(table).As("i"), q.MapTable(entries).As("e")
			id, rank := q.FieldOf(i.Rel(), "id", q.Int8()), q.FieldOf(i.Rel(), "rank", q.Int4())
			entryID, entryRank := q.FieldOf(e.Rel(), "id", q.Int8()), q.FieldOf(e.Rel(), "rank", q.Int4())
			delta := q.Input("delta", q.Int4())
			changedID, changedRank := q.Export("id", id.Expr()), q.Export("rank", rank.Expr())
			changed := q.WriteCTE("changed", q.WithReturning(q.UpdateOf(i).Set(q.Assign(rank, q.Plus(rank.Expr(), delta.Expr()))).AllRows(), changedID.Project(), changedRank.Project()))
			c := changed.As("c")
			insert := q.DefineWrite(e, "record", q.InsertInto(e).Columns(entryID.Column(), entryRank.Column()).FromSelect(
				q.Select(q.Export("id", changedID.From(c)).Project(), q.Export("rank", changedRank.From(c)).Project()).From(c)).With(changed))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				n, err := insert.Bind(s).Exec(ctx, q.BindValue(delta, int32(5)))
				require.NoError(t, err)
				require.EqualValues(t, 2, n)
				return nil
			}))
			var total int64
			require.NoError(t, db.QueryRowContext(ctx, "SELECT sum(rank) FROM "+schema+".entries").Scan(&total))
			require.EqualValues(t, 15, total)
			key := q.Input("id", q.Int8())
			effect := q.EffectCTE("appended", q.InsertInto(e).Values(q.Assign(entryID, key.Expr()), q.Assign(entryRank, delta.Expr())))
			out := q.DefineView(reg, q.ViewSpec[lockResult]{Name: "notes"})
			returning := q.DefineReturning(out, "append_and_read", q.Select(q.Project(q.Output(out, "id", q.Int8()), id.Expr())).From(i).With(effect))
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				rows, err := returning.Bind(s).All(ctx, q.BindValue(key, int64(3)), q.BindValue(delta, int32(9)))
				require.NoError(t, err)
				require.ElementsMatch(t, []lockResult{{1}, {2}}, rows)
				return nil
			}))
			shape := out.Definition().StatementShapes["append_and_read.global.write"]
			for _, access := range shape.Access {
				if access.Relation == `"`+schema+`"."entries"` {
					require.Equal(t, "INSERT", access.Command)
				}
			}
			_, err = returning.Bind(effectDirect(t, driver, reg.SQLAuthority())).All(ctx, q.BindValue(key, int64(4)), q.BindValue(delta, int32(9)))
			require.ErrorIs(t, err, q.ErrReadOnly)
			// Reading the base table in a sibling observes the statement snapshot,
			// while the CTE's own result contains the newly changed values.
			snapshot := q.DefineReturning(out, "snapshot", q.Select(q.Project(q.Output(out, "id", q.Int8()), id.Expr())).From(i).
				Where(q.Eq(rank.Expr(), delta.Expr())).With(q.EffectCTE("modified", q.UpdateOf(i).Set(q.Assign(rank, delta.Expr())).AllRows())))
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				rows, err := snapshot.Bind(s).All(ctx, q.BindValue(delta, int32(20)))
				require.NoError(t, err)
				require.Empty(t, rows)
				return nil
			}))
			require.NoError(t, db.QueryRowContext(ctx, "SELECT sum(rank) FROM "+schema+".items").Scan(&total))
			require.EqualValues(t, 40, total)
			left, right := changed.As("a"), changed.As("b")
			reused := q.DefineReturning(out, "reused", q.Select(q.Project(q.Output(out, "id", q.Int8()), changedID.From(left))).
				From(left).InnerJoin(right, q.Eq(changedID.From(left), changedID.From(right))).With(changed))
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				rows, err := reused.Bind(s).All(ctx, q.BindValue(delta, int32(1)))
				require.NoError(t, err)
				require.ElementsMatch(t, []lockResult{{1}, {2}}, rows)
				return nil
			}))
			require.NoError(t, db.QueryRowContext(ctx, "SELECT sum(rank) FROM "+schema+".items").Scan(&total))
			require.EqualValues(t, 42, total)
			jobs := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".jobs", RelationScope: q.Global()})
			j := q.MapTable(jobs).As("j")
			jobID, jobRank := q.FieldOf(j.Rel(), "id", q.Int8()), q.FieldOf(j.Rel(), "rank", q.Int4())
			matched := q.DefineWrite(j, "from_changed", q.UpdateOf(j).Set(q.Assign(jobRank, changedRank.From(c))).
				From(c, q.Eq(changedID.From(c), jobID.Expr())).AllRows().With(changed))
			require.Len(t, jobs.Definition().StatementShapes["from_changed.global.write"].Keys, 1)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				n, err := matched.Bind(s).Exec(ctx, q.BindValue(delta, int32(1)))
				require.NoError(t, err)
				require.EqualValues(t, 2, n)
				return nil
			}))
			require.NoError(t, db.QueryRowContext(ctx, "SELECT sum(rank) FROM "+schema+".jobs").Scan(&total))
			require.EqualValues(t, 44, total)
		})
	}
}

func TestWriteUnnestGroupedMatch(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			_, err := db.ExecContext(ctx, "CREATE TABLE "+schema+`.items(id bigint PRIMARY KEY, rank int NOT NULL, label text NOT NULL DEFAULT 'note'); INSERT INTO `+schema+`.items VALUES(1,0,'first'),(2,0,'second')`)
			require.NoError(t, err)
			cfg := q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL}
			if driver == "pq" {
				cfg.ArrayArg = pqdriver.Array
			}
			reg := q.NewRegistry(cfg)
			table := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".items", RelationScope: q.Global()})
			src := q.MapTable(table).As("i")
			id, rank := q.FieldOf(src.Rel(), "id", q.Int8()), q.FieldOf(src.Rel(), "rank", q.Int4())
			ids, ranks, fallback := q.Input("ids", q.ArrayOf(q.Int8())), q.Input("ranks", q.ArrayOf(q.Int4())), q.Input("fallback", q.Int4())
			itemID, itemRank := q.UnnestColumn("id", ids.Expr(), q.Int8()), q.UnnestColumn("rank", ranks.Expr(), q.Int4())
			rows := q.Unnest("u", itemID, itemRank)
			_, err = q.TryDefineWrite(src, "ambiguous", q.UpdateOf(src).Set(q.Assign(rank, q.Coalesce(itemRank.From(rows), fallback.Expr()))).From(rows, q.Eq(itemID.From(rows), id.Expr())).AllRows())
			require.ErrorContains(t, err, "proven result key")
			groupID, groupRank := q.Export("id", itemID.From(rows)), q.Export("rank", q.MaxOf(itemRank.From(rows)))
			grouped := q.Derived("g", q.Select(groupID.Project(), groupRank.Project()).From(rows).GroupBy(itemID.From(rows)))
			plan := q.DefineTableReturning(src, "grouped", q.UpdateOf(src).Set(q.Assign(rank, q.Coalesce(groupRank.From(grouped), fallback.Expr()))).
				From(grouped, q.Eq(groupID.From(grouped), id.Expr())).AllRows(), q.SelectColumns(id.Column(), rank.Column()))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				result, err := plan.Bind(s).All(ctx, q.BindValue(ids, []int64{1, 1, 2}), q.BindValue(ranks, []int32{4, 8, 6}), q.BindValue(fallback, int32(0)))
				require.NoError(t, err)
				require.ElementsMatch(t, []writeItem{{ID: 1, Rank: 8}, {ID: 2, Rank: 6}}, result)
				return nil
			}))
		})
	}
}
