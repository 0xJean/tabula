//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/plan"
	"github.com/stretchr/testify/require"
)

func TestScalarConditionalAndArithmetic(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		a, b := plan.Input("a", pg.Int4()), plan.Input("b", pg.Int4())
		x, y := plan.BindValue(a, int32(7)), plan.BindValue(b, int32(3))
		for _, c := range []struct {
			name string
			expr plan.Expr[int32]
			want int32
		}{
			{"plus", plan.Plus(a.Expr(), b.Expr()), 10}, {"minus", plan.Minus(a.Expr(), b.Expr()), 4},
			{"times", plan.Times(a.Expr(), b.Expr()), 21}, {"divide", plan.Divide(a.Expr(), b.Expr()), 2},
			{"greatest", plan.Greatest(a.Expr(), b.Expr()), 7}, {"least", plan.Least(a.Expr(), b.Expr()), 3},
			{"case", plan.Case(plan.When(plan.TypedNull(pg.Bool()), b.Expr())).Else(a.Expr()), 7},
			{"nullif_different", plan.NullIf(a.Expr(), b.Expr()), 7},
		} {
			scalarCheck(f, c.name, pg.Int4(), c.expr, new(c.want), x, y)
		}
		scalarCheck(f, "coalesce", pg.Int4(), plan.Coalesce(plan.TypedNull(pg.Int4()), a.Expr()), new(int32(7)), x)
		scalarCheck(f, "nullif_equal", pg.Int4(), plan.NullIf(a.Expr(), a.Expr()), nil, x)
		scalarCheck(f, "nullif_null_lhs", pg.Int4(), plan.NullIf(plan.TypedNull(pg.Int4()), a.Expr()), nil, x)
		scalarCheck(f, "nullif_null_rhs", pg.Int4(), plan.NullIf(a.Expr(), plan.TypedNull(pg.Int4())), new(int32(7)), x)
		scalarCheck(f, "case_else_null", pg.Int4(), plan.Case(plan.When(plan.TypedNull(pg.Bool()), a.Expr())).ElseNull(), nil, x)
		big := plan.Input("big", pg.Int8())
		scalarCheck(f, "mixed_integer", pg.Int8(), plan.PlusAs(pg.Int8(), a.Expr(), big.Expr()), new(int64(4294967303)), x, plan.BindValue(big, int64(4294967296)))
		arr := plan.Input("array", pg.ArrayOf(pg.Int4()))
		scalarCheck(f, "nullif_array_equal", pg.Text(), plan.Cast(pg.Text(), plan.NullIf(arr.Expr(), arr.Expr())), nil, plan.BindValue(arr, []int32{1, 2}))
		scalarCheck(f, "nullif_array_empty", pg.Text(), plan.Cast(pg.Text(), plan.NullIf(arr.Expr(), arr.Expr())), nil, plan.BindValue(arr, []int32{}))
		scalarCheck(f, "nullif_array_null", pg.Text(), plan.Cast(pg.Text(), plan.NullIf(arr.Expr(), arr.Expr())), nil, plan.BindValue(arr, []int32(nil)))
		scalarCheck(f, "nullif_array_null_rhs", pg.Text(), plan.Cast(pg.Text(), plan.NullIf(arr.Expr(), plan.TypedNull(pg.ArrayOf(pg.Int4())))), new(`{1,2}`), plan.BindValue(arr, []int32{1, 2}))
		scalarCheck(f, "nullif_array_null_elements", pg.Text(), plan.Cast(pg.Text(), plan.NullIf(plan.ArrayExpr(plan.TypedNull(pg.Int4())), plan.ArrayExpr(a.Expr()))), new(`{NULL}`), x)
		scalarCheck(f, "nullif_aggregate", pg.Int8(), plan.NullIf(plan.CountAll(), big.Expr()), nil, plan.BindValue(big, int64(1)))
		text := plan.Input("text", pg.Text())
		scalarCheck(f, "nullif_immutable_array_volatile_rhs", pg.Text(), plan.Cast(pg.Text(), plan.NullIf(plan.ArrayExpr(text.Expr()), plan.ArrayExpr(plan.Cast(pg.Text(), plan.GenRandomUUID())))), new(`{x}`), plan.BindValue(text, "x"))
		scalarCheck(f, "nullif_stable_array_volatile_rhs", pg.Text(), plan.Cast(pg.Text(), plan.NullIf(plan.ArrayExpr(plan.Concat(text.Expr())), plan.ArrayExpr(plan.Cast(pg.Text(), plan.GenRandomUUID())))), new(`{x}`), plan.BindValue(text, "x"))
		// The native equality in NULLIF cannot be replaced by search_path.
		baseRun := f.run
		f.run = func(ctx context.Context, body callback) error {
			return baseRun(ctx, func(ctx context.Context, s session) error {
				_, err := s.Exec(ctx, `CREATE SCHEMA demo;
CREATE FUNCTION demo.eq(int4,int4) RETURNS bool LANGUAGE sql IMMUTABLE AS 'SELECT true';
CREATE OPERATOR demo.= (FUNCTION=demo.eq,LEFTARG=int4,RIGHTARG=int4);
SET LOCAL search_path=demo,pg_catalog`)
				if err != nil {
					return err
				}
				defer func() { _, _ = s.Exec(ctx, "DROP SCHEMA demo CASCADE") }()
				return body(ctx, s)
			})
		}
		scalarCheck(f, "qualified_nullif", pg.Int4(), plan.NullIf(a.Expr(), b.Expr()), new(int32(7)), x, y)
	})
}

func TestScalarNullIfVolatileRows(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		uuid := plan.NullIf(plan.GenRandomUUID(), plan.TypedNull(pg.UUID[string]()))
		p := scalarPlan(f, pg.UUID[string](), uuid)
		// Insert via the same declared physical source, then test repeated rows.
		var table string
		for _, d := range f.reg.Defined() {
			table = d.Name
		}
		_, err := f.db.ExecContext(f.t.Context(), "INSERT INTO "+table+" VALUES (2),(3),(4),(5) ON CONFLICT DO NOTHING")
		require.NoError(f.t, err)
		require.NoError(f.t, f.run(f.t.Context(), func(ctx context.Context, s session) error {
			rows, err := p.Bind(s).Find(ctx)
			require.NoError(f.t, err)
			require.Len(f.t, rows, 5)
			seen := map[string]bool{}
			for _, row := range rows {
				require.NotNil(f.t, row.Value)
				require.False(f.t, seen[*row.Value])
				seen[*row.Value] = true
			}
			return nil
		}))
		// Aggregate expressions stay in their original SELECT binding level.
		big := plan.Input("count", pg.Int8())
		out := q.DefineView(f.reg, q.ViewSpec[scalarValue[int64]]{Name: "groups"})
		id := plan.FieldOf(f.src.Rel(), "id", pg.Int8())
		grouped := plan.DefineRead(out, "counts", plan.Select(plan.Project(plan.Output(out, "value", pg.Int8()), plan.NullIf(plan.CountAll(), big.Expr()))).From(f.src).GroupBy(id.Expr()))
		require.NoError(f.t, f.run(f.t.Context(), func(ctx context.Context, s session) error {
			rows, err := grouped.Bind(s).Find(ctx, plan.BindValue(big, int64(1)))
			require.NoError(f.t, err)
			require.Len(f.t, rows, 5)
			for _, row := range rows {
				require.Nil(f.t, row.Value)
			}
			return nil
		}))
	})
}
