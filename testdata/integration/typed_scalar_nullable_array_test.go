//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/stretchr/testify/require"
)

func TestScalarNullableArrayResults(t *testing.T) {
	type item struct {
		ID   int64  `db:"id,key"`
		Rank *int32 `db:"rank"`
	}
	type result struct {
		Values []*int32 `db:"values,array"`
	}
	type truth struct {
		Value *bool `db:"value"`
	}
	db, pool := databases(t)
	tableName := typesSchema(t, db) + ".items"
	_, err := db.ExecContext(t.Context(), "CREATE TABLE "+tableName+" (id bigint PRIMARY KEY,rank int4); INSERT INTO "+tableName+" VALUES (1,0),(2,7),(3,NULL)")
	require.NoError(t, err)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			cfg := q.RegistryConfig[struct{}]{ArrayColumn: pgxdriver.ArrayColumn}
			if driver == "pq" {
				cfg.ArrayArg, cfg.ArrayColumn = pqdriver.Array, pqdriver.ArrayColumn
			}
			reg := q.NewRegistry(cfg)
			table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: tableName, RelationScope: q.Global()})
			layout := plan.MapTable(table)
			left, right := layout.As("i"), layout.As("r")
			id, rank := plan.MappedColumn[int64](layout, "id"), plan.MappedColumn[int32](layout, "rank")
			kind := pg.ArrayOfNullable(pg.Int4())
			out := q.DefineView(reg, q.ViewSpec[result]{Name: "entries"})
			target := plan.Output(out, "values", kind)
			plain := plan.DefineRead(out, "plain", plan.Select(plan.Project(target, plan.ArrayExprAs(kind, rank.On(left.Rel()).Expr()))).From(left).OrderBy(plan.Asc(id.On(left.Rel()))))
			match := plan.Input("match", pg.Int8())
			joined := plan.DefineRead(out, "joined", plan.Select(plan.Project(target, plan.ArrayExprAs(kind, rank.On(right.Rel()).Expr()))).From(left).
				LeftJoin(right, plan.And(plan.Eq(id.On(left.Rel()).Expr(), id.On(right.Rel()).Expr()), plan.Eq(id.On(right.Rel()).Expr(), match.Expr()))).OrderBy(plan.Asc(id.On(left.Rel()))))
			input, element := plan.Input("array", kind), plan.Input("element", pg.Int4())
			joinedRemove := plan.DefineRead(out, "joined_remove", plan.Select(plan.Project(target, plan.ArrayRemove(plan.ArrayExprAs(kind, rank.On(right.Rel()).Expr()), element.Expr()))).From(left).
				LeftJoin(right, plan.And(plan.Eq(id.On(left.Rel()).Expr(), id.On(right.Rel()).Expr()), plan.Eq(id.On(right.Rel()).Expr(), match.Expr()))).OrderBy(plan.Asc(id.On(left.Rel()))))
			remove := plan.DefineRead(out, "remove", plan.Select(plan.Project(target, plan.ArrayRemove(input.Expr(), element.Expr()))).From(left).Where(plan.Eq(id.On(left.Rel()).Expr(), match.Expr())))
			removeNull := plan.DefineRead(out, "remove_null", plan.Select(plan.Project(target, plan.ArrayRemove(input.Expr(), plan.TypedNull(pg.Int4())))).From(left).Where(plan.Eq(id.On(left.Rel()).Expr(), match.Expr())))
			boolOut := q.DefineView(reg, q.ViewSpec[truth]{Name: "notes"})
			all := plan.DefineRead(boolOut, "all", plan.Select(plan.Project(plan.Output(boolOut, "value", pg.Bool()), plan.NotEqualAll(element.Expr(), input.Expr()))).From(left).Where(plan.Eq(id.On(left.Rel()).Expr(), match.Expr())))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				rows, err := plain.Bind(s).Find(ctx)
				require.NoError(t, err)
				require.Equal(t, []result{{[]*int32{new(int32(0))}}, {[]*int32{new(int32(7))}}, {[]*int32{nil}}}, rows)
				rows, err = joined.Bind(s).Find(ctx, plan.BindValue(match, int64(1)))
				require.NoError(t, err)
				require.Equal(t, []result{{[]*int32{new(int32(0))}}, {[]*int32{nil}}, {[]*int32{nil}}}, rows)
				rows, err = joinedRemove.Bind(s).Find(ctx, plan.BindValue(match, int64(1)), plan.BindValue(element, int32(0)))
				require.NoError(t, err)
				require.Equal(t, []result{{[]*int32{}}, {[]*int32{nil}}, {[]*int32{nil}}}, rows)
				binding := plan.BindValue(input, []*int32{new(int32(0)), nil, new(int32(7))})
				for range 2 {
					rows, err = remove.Bind(s).Find(ctx, binding, plan.BindValue(element, int32(0)), plan.BindValue(match, int64(1)))
					require.NoError(t, err)
					require.Equal(t, []*int32{nil, new(int32(7))}, rows[0].Values)
				}
				rows, err = removeNull.Bind(s).Find(ctx, binding, plan.BindValue(match, int64(1)))
				require.NoError(t, err)
				require.Equal(t, []*int32{new(int32(0)), new(int32(7))}, rows[0].Values)
				for _, empty := range [][]*int32{nil, {}} {
					rows, err = remove.Bind(s).Find(ctx, plan.BindValue(input, empty), plan.BindValue(element, int32(0)), plan.BindValue(match, int64(1)))
					require.NoError(t, err)
					require.Equal(t, empty, rows[0].Values)
				}
				for _, c := range []struct {
					values []*int32
					want   *bool
				}{
					{[]*int32{nil, new(int32(7))}, nil},
					{[]*int32{nil, new(int32(0))}, new(false)},
					{[]*int32{}, new(true)}, {nil, nil},
				} {
					rows, err := all.Bind(s).Find(ctx, plan.BindValue(input, c.values), plan.BindValue(element, int32(0)), plan.BindValue(match, int64(1)))
					require.NoError(t, err)
					require.Equal(t, c.want, rows[0].Value)
				}
				return nil
			}))
		})
	}
}
