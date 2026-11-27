//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
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
			src := q.MapTable(table).As("i")
			id, rank := q.FieldOf(src.Rel(), "id", q.Int8()), q.FieldOf(src.Rel(), "rank", q.Int4())
			key, value := q.Input("id", q.Int8()), q.Input("rank", q.Int4())
			plan := q.DefineWrite(src, "overwrite", q.UpdateOf(src).Set(q.Assign(rank, value.Expr())).Where(q.Ge(id.Expr(), key.Expr())))
			out := q.DefineView(reg, q.ViewSpec[lockResult]{Name: "entries"})
			finite := q.DefineUpdateReturning(src, out, "finite", q.MutationSpec{RowFilter: q.RowFilter{
				Where: q.Ge(id.Expr(), key.Expr()), Optional: []q.RowPredicate{{Name: "equal", When: q.Eq(id.Expr(), key.Expr())}},
			}}, []q.RowSet{{Name: "value", Assignments: []q.RowAssignment{q.Assign(rank, value.Expr())}}, {Name: "default", Assignments: []q.RowAssignment{q.DefaultOf(rank)}}},
				q.Project(q.Output(out, "id", q.Int8()), id.Expr()))
			run := authorizedEffectRunner(t, driver, db, pool, role, reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				n, err := plan.Bind(s).Exec(ctx, q.BindValue(key, int64(1)), q.BindValue(value, int32(9)))
				require.NoError(t, err)
				require.EqualValues(t, 3, n)
				rows, err := finite.Bind(s).UpdateMany(ctx, "default", []string{"equal"}, q.BindValue(key, int64(2)))
				require.NoError(t, err)
				require.Equal(t, []lockResult{{ID: 2}}, rows)
				seen := 0
				for _, err := range finite.Bind(s).UpdateIter(ctx, "value", nil, q.BindValue(key, int64(1)), q.BindValue(value, int32(7))) {
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
			decodeFailure := q.DefineReturning(badOut, "overflow", q.WithReturning(q.UpdateOf(src).Set(q.Assign(rank, value.Expr())).AllRows(),
				q.Project(q.Output(badOut, "id", small), q.Cast(small, id.Expr()))))
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				rows, err := decodeFailure.Bind(s).All(ctx, q.BindValue(value, int32(10)))
				require.Error(t, err)
				require.Nil(t, rows)
				// Returning a handled client error must not commit the successful UPDATE.
				return nil
			}))
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+schema+".items WHERE rank=7").Scan(&count))
			require.Equal(t, 3, count)
			exec("REVOKE UPDATE(rank) ON " + schema + ".items FROM " + role)
			err := run(ctx, func(ctx context.Context, s session) error {
				_, err := finite.Bind(s).UpdateMany(ctx, "default", nil, q.BindValue(key, int64(1)))
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
			i, n := q.MapTable(table).As("i"), q.MapTable(notes).As("n")
			id, rank, label := q.FieldOf(i.Rel(), "id", q.Int8()), q.FieldOf(i.Rel(), "rank", q.Int4()), q.FieldOf(i.Rel(), "label", q.Text())
			otherID, otherRank, otherLabel := q.FieldOf(n.Rel(), "id", q.Int8()), q.FieldOf(n.Rel(), "rank", q.Int4()), q.FieldOf(n.Rel(), "label", q.Text())
			key := q.Input("id", q.Int8())
			selection := q.Select(q.Export("id", otherID.Expr()).Project(), q.Export("rank", otherRank.Expr()).Project(), q.Export("label", otherLabel.Expr()).Project()).From(n).Where(q.Ge(otherID.Expr(), key.Expr()))
			insert := q.DefineWrite(i, "copy", q.InsertInto(i).Columns(id.Column(), rank.Column(), label.Column()).FromSelect(selection))
			update := q.DefineTableReturning(i, "matched", q.UpdateOf(i).Set(q.Assign(rank, otherRank.Expr())).From(n, q.Eq(otherID.Expr(), id.Expr())).AllRows(), q.SelectColumns(id.Column(), rank.Column()))
			targetResult := q.DefineView(reg, q.ViewSpec[lockResult]{Name: "groups"})
			offset := q.Input("offset", q.Int8())
			deleteRows := q.DefineReturning(targetResult, "remove", q.WithReturning(q.DeleteFrom(i).Using(n, q.Ge(otherID.Expr(), id.Expr())).Where(q.Ge(id.Expr(), key.Expr())),
				q.Project(q.Output(targetResult, "id", q.Int8()), q.Plus(id.Expr(), offset.Expr()))))
			type joinedRow struct {
				ID   int64 `db:"id"`
				Rank int32 `db:"rank"`
			}
			out := q.DefineView(reg, q.ViewSpec[joinedRow]{Name: "entries"})
			projections := []q.Projection{q.Project(q.Output(out, "id", q.Int8()), id.Expr()), q.Project(q.Output(out, "rank", q.Int4()), otherRank.Expr())}
			_, err = q.TryDefineReturning(out, "ambiguous", q.WithReturning(q.DeleteFrom(i).Using(n, q.Ge(otherID.Expr(), id.Expr())).AllRows(), projections...))
			require.ErrorContains(t, err, "unique key")
			joined := q.DefineReturning(out, "joined", q.WithReturning(q.DeleteFrom(i).Using(n, q.Eq(otherID.Expr(), id.Expr())).AllRows(), projections...))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				n, err := insert.Bind(s).Exec(ctx, q.BindValue(key, int64(9)))
				require.NoError(t, err)
				require.Zero(t, n)
				n, err = insert.Bind(s).Exec(ctx, q.BindValue(key, int64(1)))
				require.NoError(t, err)
				require.EqualValues(t, 3, n)
				rows, err := update.Bind(s).All(ctx)
				require.NoError(t, err)
				require.ElementsMatch(t, []writeItem{{ID: 1, Rank: 2}, {ID: 2, Rank: 3}, {ID: 3, Rank: 4}}, rows)
				deleted, err := deleteRows.Bind(s).All(ctx, q.BindValue(key, int64(2)), q.BindValue(offset, int64(10)))
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
