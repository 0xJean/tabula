//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	tplan "github.com/0xJean/tabula/plan"
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
			i, e := tplan.MapTable(table).As("i"), tplan.MapTable(entries).As("e")
			id, rank := tplan.FieldOf(i.Rel(), "id", tplan.Int8()), tplan.FieldOf(i.Rel(), "rank", tplan.Int4())
			entryID, entryRank := tplan.FieldOf(e.Rel(), "id", tplan.Int8()), tplan.FieldOf(e.Rel(), "rank", tplan.Int4())
			delta := tplan.Input("delta", tplan.Int4())
			changedID, changedRank := tplan.Export("id", id.Expr()), tplan.Export("rank", rank.Expr())
			changed := tplan.WriteCTE("changed", tplan.WithReturning(tplan.UpdateOf(i).Set(tplan.Assign(rank, tplan.Plus(rank.Expr(), delta.Expr()))).AllRows(), changedID.Project(), changedRank.Project()))
			c := changed.As("c")
			insert := tplan.DefineWrite(e, "record", tplan.InsertInto(e).Columns(entryID.Column(), entryRank.Column()).FromSelect(
				tplan.Select(tplan.Export("id", changedID.From(c)).Project(), tplan.Export("rank", changedRank.From(c)).Project()).From(c)).With(changed))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				n, err := insert.Bind(s).Exec(ctx, tplan.BindValue(delta, int32(5)))
				require.NoError(t, err)
				require.EqualValues(t, 2, n)
				return nil
			}))
			var total int64
			require.NoError(t, db.QueryRowContext(ctx, "SELECT sum(rank) FROM "+schema+".entries").Scan(&total))
			require.EqualValues(t, 15, total)
			key := tplan.Input("id", tplan.Int8())
			effect := tplan.EffectCTE("appended", tplan.InsertInto(e).Values(tplan.Assign(entryID, key.Expr()), tplan.Assign(entryRank, delta.Expr())))
			out := q.DefineView(reg, q.ViewSpec[lockResult]{Name: "notes"})
			returning := tplan.DefineReturning(out, "append_and_read", tplan.Select(tplan.Project(tplan.Output(out, "id", tplan.Int8()), id.Expr())).From(i).With(effect))
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				rows, err := returning.Bind(s).All(ctx, tplan.BindValue(key, int64(3)), tplan.BindValue(delta, int32(9)))
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
			_, err = returning.Bind(effectDirect(t, driver, reg.SQLAuthority())).All(ctx, tplan.BindValue(key, int64(4)), tplan.BindValue(delta, int32(9)))
			require.ErrorIs(t, err, q.ErrReadOnly)
			// Reading the base table in a sibling observes the statement snapshot,
			// while the CTE's own result contains the newly changed values.
			snapshot := tplan.DefineReturning(out, "snapshot", tplan.Select(tplan.Project(tplan.Output(out, "id", tplan.Int8()), id.Expr())).From(i).
				Where(tplan.Eq(rank.Expr(), delta.Expr())).With(tplan.EffectCTE("modified", tplan.UpdateOf(i).Set(tplan.Assign(rank, delta.Expr())).AllRows())))
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				rows, err := snapshot.Bind(s).All(ctx, tplan.BindValue(delta, int32(20)))
				require.NoError(t, err)
				require.Empty(t, rows)
				return nil
			}))
			require.NoError(t, db.QueryRowContext(ctx, "SELECT sum(rank) FROM "+schema+".items").Scan(&total))
			require.EqualValues(t, 40, total)
			left, right := changed.As("a"), changed.As("b")
			reused := tplan.DefineReturning(out, "reused", tplan.Select(tplan.Project(tplan.Output(out, "id", tplan.Int8()), changedID.From(left))).
				From(left).InnerJoin(right, tplan.Eq(changedID.From(left), changedID.From(right))).With(changed))
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				rows, err := reused.Bind(s).All(ctx, tplan.BindValue(delta, int32(1)))
				require.NoError(t, err)
				require.ElementsMatch(t, []lockResult{{1}, {2}}, rows)
				return nil
			}))
			require.NoError(t, db.QueryRowContext(ctx, "SELECT sum(rank) FROM "+schema+".items").Scan(&total))
			require.EqualValues(t, 42, total)
			jobs := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".jobs", RelationScope: q.Global()})
			j := tplan.MapTable(jobs).As("j")
			jobID, jobRank := tplan.FieldOf(j.Rel(), "id", tplan.Int8()), tplan.FieldOf(j.Rel(), "rank", tplan.Int4())
			matched := tplan.DefineWrite(j, "from_changed", tplan.UpdateOf(j).Set(tplan.Assign(jobRank, changedRank.From(c))).
				From(c, tplan.Eq(changedID.From(c), jobID.Expr())).AllRows().With(changed))
			require.Len(t, jobs.Definition().StatementShapes["from_changed.global.write"].Keys, 1)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				n, err := matched.Bind(s).Exec(ctx, tplan.BindValue(delta, int32(1)))
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
			src := tplan.MapTable(table).As("i")
			id, rank := tplan.FieldOf(src.Rel(), "id", tplan.Int8()), tplan.FieldOf(src.Rel(), "rank", tplan.Int4())
			ids, ranks, fallback := tplan.Input("ids", q.ArrayOf(tplan.Int8())), tplan.Input("ranks", q.ArrayOf(tplan.Int4())), tplan.Input("fallback", tplan.Int4())
			itemID, itemRank := tplan.UnnestColumn("id", ids.Expr(), tplan.Int8()), tplan.UnnestColumn("rank", ranks.Expr(), tplan.Int4())
			rows := tplan.Unnest("u", itemID, itemRank)
			_, err = tplan.TryDefineWrite(src, "ambiguous", tplan.UpdateOf(src).Set(tplan.Assign(rank, tplan.Coalesce(itemRank.From(rows), fallback.Expr()))).From(rows, tplan.Eq(itemID.From(rows), id.Expr())).AllRows())
			require.ErrorContains(t, err, "proven result key")
			groupID, groupRank := tplan.Export("id", itemID.From(rows)), tplan.Export("rank", tplan.MaxOf(itemRank.From(rows)))
			grouped := tplan.Derived("g", tplan.Select(groupID.Project(), groupRank.Project()).From(rows).GroupBy(itemID.From(rows)))
			plan := tplan.DefineTableReturning(src, "grouped", tplan.UpdateOf(src).Set(tplan.Assign(rank, tplan.Coalesce(groupRank.From(grouped), fallback.Expr()))).
				From(grouped, tplan.Eq(groupID.From(grouped), id.Expr())).AllRows(), tplan.SelectColumns(id.Column(), rank.Column()))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				result, err := plan.Bind(s).All(ctx, tplan.BindValue(ids, []int64{1, 1, 2}), tplan.BindValue(ranks, []int32{4, 8, 6}), tplan.BindValue(fallback, int32(0)))
				require.NoError(t, err)
				require.ElementsMatch(t, []writeItem{{ID: 1, Rank: 8}, {ID: 2, Rank: 6}}, result)
				return nil
			}))
		})
	}
}
