//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/stretchr/testify/require"
)

func TestWriteReturningFailureAndFiniteChoices(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			role := writeRole(t, db, schema)
			exec := func(sql string) { t.Helper(); _, err := db.ExecContext(ctx, sql); require.NoError(t, err) }
			exec("CREATE TABLE " + schema + `.items (id bigint PRIMARY KEY, rank int NOT NULL DEFAULT 1, label text NOT NULL DEFAULT 'note');
INSERT INTO ` + schema + `.items VALUES(1,1,'first'),(2,2,'second'),(99999,3,'third');
GRANT SELECT(id), UPDATE(rank) ON ` + schema + ".items TO " + role)
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			table := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".items", RelationScope: q.Global(), WriteMode: q.WriteUpdateOnly, UpdateColumns: []string{"rank"}})
			src := tplan.MapTable(table).As("i")
			id, rank := tplan.FieldOf(src.Rel(), "id", tplan.Int8()), tplan.FieldOf(src.Rel(), "rank", tplan.Int4())
			key, value := tplan.Input("id", tplan.Int8()), tplan.Input("rank", tplan.Int4())
			plan := tplan.DefineWrite(src, "overwrite", tplan.UpdateOf(src).Set(tplan.Assign(rank, value.Expr())).Where(tplan.Ge(id.Expr(), key.Expr())))
			out := q.DefineView(reg, q.ViewSpec[lockResult]{Name: "entries"})
			finite := tplan.DefineUpdateReturning(src, out, "finite", tplan.MutationSpec{RowFilter: tplan.RowFilter{
				Where: tplan.Ge(id.Expr(), key.Expr()), Optional: []tplan.RowPredicate{{Name: "equal", When: tplan.Eq(id.Expr(), key.Expr())}},
			}}, []tplan.RowSet{{Name: "value", Assignments: []tplan.RowAssignment{tplan.Assign(rank, value.Expr())}}, {Name: "default", Assignments: []tplan.RowAssignment{tplan.DefaultOf(rank)}}},
				tplan.Project(tplan.Output(out, "id", tplan.Int8()), id.Expr()))
			run := authorizedEffectRunner(t, driver, db, pool, role, reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				n, err := plan.Bind(s).Exec(ctx, tplan.BindValue(key, int64(1)), tplan.BindValue(value, int32(9)))
				require.NoError(t, err)
				require.EqualValues(t, 3, n)
				rows, err := finite.Bind(s).UpdateMany(ctx, "default", []string{"equal"}, tplan.BindValue(key, int64(2)))
				require.NoError(t, err)
				require.Equal(t, []lockResult{{ID: 2}}, rows)
				seen := 0
				for _, err := range finite.Bind(s).UpdateIter(ctx, "value", nil, tplan.BindValue(key, int64(1)), tplan.BindValue(value, int32(7))) {
					require.NoError(t, err)
					seen++
					break
				}
				require.Equal(t, 1, seen)
				return nil
			}))
			var count int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+schema+".items WHERE rank=7").Scan(&count))
			require.Equal(t, 3, count)
			type smallID struct {
				ID int16 `db:"id"`
			}
			small := q.CatalogType[int16]("pg_catalog", "int8", nil)
			badOut := q.DefineView(reg, q.ViewSpec[smallID]{Name: "notes"})
			decodeFailure := tplan.DefineReturning(badOut, "overflow", tplan.WithReturning(tplan.UpdateOf(src).Set(tplan.Assign(rank, value.Expr())).AllRows(),
				tplan.Project(tplan.Output(badOut, "id", small), tplan.Cast(small, id.Expr()))))
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				rows, err := decodeFailure.Bind(s).All(ctx, tplan.BindValue(value, int32(10)))
				require.Error(t, err)
				require.Nil(t, rows)
				// Returning a handled client error must not commit the successful UPDATE.
				return nil
			}))
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+schema+".items WHERE rank=7").Scan(&count))
			require.Equal(t, 3, count)
			exec("REVOKE UPDATE(rank) ON " + schema + ".items FROM " + role)
			err := run(ctx, func(ctx context.Context, s session) error {
				_, err := finite.Bind(s).UpdateMany(ctx, "default", nil, tplan.BindValue(key, int64(1)))
				return err
			})
			require.Equal(t, "42501", q.SQLState(err))
		})
	}
}

func TestWriteDeleteUsingAndInsertSelect(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			_, err := db.ExecContext(ctx, "CREATE TABLE "+schema+`.items(id bigint PRIMARY KEY, rank int NOT NULL, label text NOT NULL);
CREATE TABLE `+schema+`.notes(LIKE `+schema+`.items INCLUDING ALL);
INSERT INTO `+schema+`.notes VALUES(1,2,'first'),(2,3,'second'),(3,4,'third')`)
			require.NoError(t, err)
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			table := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".items", RelationScope: q.Global(), AllowDelete: true})
			notes := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".notes", RelationScope: q.Global(), ReadOnly: true})
			i, n := tplan.MapTable(table).As("i"), tplan.MapTable(notes).As("n")
			id, rank, label := tplan.FieldOf(i.Rel(), "id", tplan.Int8()), tplan.FieldOf(i.Rel(), "rank", tplan.Int4()), tplan.FieldOf(i.Rel(), "label", tplan.Text())
			otherID, otherRank, otherLabel := tplan.FieldOf(n.Rel(), "id", tplan.Int8()), tplan.FieldOf(n.Rel(), "rank", tplan.Int4()), tplan.FieldOf(n.Rel(), "label", tplan.Text())
			key := tplan.Input("id", tplan.Int8())
			selection := tplan.Select(tplan.Export("id", otherID.Expr()).Project(), tplan.Export("rank", otherRank.Expr()).Project(), tplan.Export("label", otherLabel.Expr()).Project()).From(n).Where(tplan.Ge(otherID.Expr(), key.Expr()))
			insert := tplan.DefineWrite(i, "copy", tplan.InsertInto(i).Columns(id.Column(), rank.Column(), label.Column()).FromSelect(selection))
			update := tplan.DefineTableReturning(i, "matched", tplan.UpdateOf(i).Set(tplan.Assign(rank, otherRank.Expr())).From(n, tplan.Eq(otherID.Expr(), id.Expr())).AllRows(), tplan.SelectColumns(id.Column(), rank.Column()))
			targetResult := q.DefineView(reg, q.ViewSpec[lockResult]{Name: "groups"})
			offset := tplan.Input("offset", tplan.Int8())
			deleteRows := tplan.DefineReturning(targetResult, "remove", tplan.WithReturning(tplan.DeleteFrom(i).Using(n, tplan.Ge(otherID.Expr(), id.Expr())).Where(tplan.Ge(id.Expr(), key.Expr())),
				tplan.Project(tplan.Output(targetResult, "id", tplan.Int8()), tplan.Plus(id.Expr(), offset.Expr()))))
			type joinedRow struct {
				ID   int64 `db:"id"`
				Rank int32 `db:"rank"`
			}
			out := q.DefineView(reg, q.ViewSpec[joinedRow]{Name: "entries"})
			projections := []tplan.Projection{tplan.Project(tplan.Output(out, "id", tplan.Int8()), id.Expr()), tplan.Project(tplan.Output(out, "rank", tplan.Int4()), otherRank.Expr())}
			_, err = tplan.TryDefineReturning(out, "ambiguous", tplan.WithReturning(tplan.DeleteFrom(i).Using(n, tplan.Ge(otherID.Expr(), id.Expr())).AllRows(), projections...))
			require.ErrorContains(t, err, "unique key")
			joined := tplan.DefineReturning(out, "joined", tplan.WithReturning(tplan.DeleteFrom(i).Using(n, tplan.Eq(otherID.Expr(), id.Expr())).AllRows(), projections...))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				n, err := insert.Bind(s).Exec(ctx, tplan.BindValue(key, int64(9)))
				require.NoError(t, err)
				require.Zero(t, n)
				n, err = insert.Bind(s).Exec(ctx, tplan.BindValue(key, int64(1)))
				require.NoError(t, err)
				require.EqualValues(t, 3, n)
				rows, err := update.Bind(s).All(ctx)
				require.NoError(t, err)
				require.ElementsMatch(t, []writeItem{{ID: 1, Rank: 2}, {ID: 2, Rank: 3}, {ID: 3, Rank: 4}}, rows)
				deleted, err := deleteRows.Bind(s).All(ctx, tplan.BindValue(key, int64(2)), tplan.BindValue(offset, int64(10)))
				require.NoError(t, err)
				require.ElementsMatch(t, []lockResult{{ID: 12}, {ID: 13}}, deleted)
				result, err := joined.Bind(s).All(ctx)
				require.NoError(t, err)
				require.Equal(t, []joinedRow{{ID: 1, Rank: 2}}, result)
				return nil
			}))
		})
	}
}
