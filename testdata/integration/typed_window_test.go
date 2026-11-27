//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/stretchr/testify/require"
)

type windowValue[V any] struct {
	ID    int64 `db:"id"`
	Value *V    `db:"value"`
}

func windowPlan[V any](f *scalarFixture, kind q.PGType[V], value tplan.Expr[V]) *tplan.ReadPlan[windowValue[V], struct{}] {
	f.t.Helper()
	f.serial++
	out := q.DefineView(f.reg, q.ViewSpec[windowValue[V]]{Name: fmt.Sprintf("entries_%d", f.serial)})
	id := tplan.FieldOf(f.src.Rel(), "id", pg.Int8())
	plan := tplan.DefineRead(out, "windowed", tplan.Select(tplan.Project(tplan.Output(out, "id", pg.Int8()), id.Expr()), tplan.Project(tplan.Output(out, "value", kind), value)).From(f.src).OrderBy(tplan.Asc(id)))
	conn, err := f.pool.Acquire(f.t.Context())
	require.NoError(f.t, err)
	defer conn.Release()
	for key, query := range out.Definition().Statements {
		desc, err := conn.Conn().Prepare(f.t.Context(), "", query)
		require.NoError(f.t, err, query)
		shape := out.Definition().StatementShapes[key]
		require.Len(f.t, desc.Fields, 2)
		for i, field := range shape.Result {
			var oid uint32
			require.NoError(f.t, f.db.QueryRowContext(f.t.Context(), "SELECT $1::regtype::oid", field.Type).Scan(&oid))
			require.Equal(f.t, oid, desc.Fields[i].DataTypeOID)
		}
		for _, privilege := range shape.Privileges {
			if privilege.Privilege == "EXECUTE" {
				var exists bool
				require.NoError(f.t, f.db.QueryRowContext(f.t.Context(), "SELECT to_regprocedure($1) IS NOT NULL", privilege.Function).Scan(&exists))
				require.True(f.t, exists, privilege.Function)
			}
		}
	}
	return plan
}

func windowCheck[V any](f *scalarFixture, name string, kind q.PGType[V], value tplan.Expr[V], want []*V, args ...tplan.Binding) {
	f.t.Helper()
	f.t.Run(name, func(t *testing.T) {
		plan := windowPlan(f, kind, value)
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			rows, err := plan.Bind(s).Find(ctx, args...)
			require.NoError(t, err)
			require.Len(t, rows, len(want))
			for i, row := range rows {
				require.EqualValues(t, i+1, row.ID)
				require.Equal(t, want[i], row.Value, "row %d", i+1)
			}
			return nil
		}))
	})
}

func windowPointers[V any](values ...V) []*V {
	out := make([]*V, len(values))
	for i, v := range values {
		out[i] = new(v)
	}
	return out
}

func windowRows(f *scalarFixture) {
	f.t.Helper()
	for _, d := range f.reg.Defined() {
		_, err := f.db.ExecContext(f.t.Context(), "INSERT INTO "+d.Name+" SELECT generate_series(2,6) ON CONFLICT DO NOTHING")
		require.NoError(f.t, err)
	}
}

func TestWindowRankingAndValues(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		id := tplan.FieldOf(f.src.Rel(), "id", pg.Int8())
		two := tplan.Input("two", pg.Int8())
		offset, fallback := tplan.Input("offset", pg.Int4()), tplan.Input("fallback", pg.Int8())
		spec := tplan.WindowSpec{Order: []tplan.RowTerm{tplan.AscExpr(tplan.Divide(id.Expr(), two.Expr()))}}
		arg := tplan.BindValue(two, int64(2))
		windowCheck(f, "row_number", pg.Int8(), tplan.Over(tplan.RowNumber(), tplan.WindowSpec{Order: []tplan.RowTerm{tplan.Asc(id)}}), windowPointers[int64](1, 2, 3, 4, 5, 6))
		windowCheck(f, "rank", pg.Int8(), tplan.Over(tplan.Rank(), spec), windowPointers[int64](1, 2, 2, 4, 4, 6), arg)
		windowCheck(f, "dense_rank", pg.Int8(), tplan.Over(tplan.DenseRank(), spec), windowPointers[int64](1, 2, 2, 3, 3, 4), arg)
		windowCheck(f, "percent_rank", pg.Float8(), tplan.Over(tplan.PercentRank(), spec), windowPointers[float64](0, .2, .2, .6, .6, 1), arg)
		windowCheck(f, "cume_dist", pg.Float8(), tplan.Over(tplan.CumeDist(), spec), windowPointers[float64](1.0/6, .5, .5, 5.0/6, 5.0/6, 1), arg)
		windowCheck(f, "ntile", pg.Int4(), tplan.Over(tplan.NTile(offset.Expr()), tplan.WindowSpec{Order: []tplan.RowTerm{tplan.Asc(id)}}), windowPointers[int32](1, 1, 1, 2, 2, 2), tplan.BindValue(offset, int32(2)))
		spec = tplan.WindowSpec{Name: "values", Order: []tplan.RowTerm{tplan.Asc(id)}, Frame: tplan.WholePartition()}
		windowCheck(f, "lag", pg.Int8(), tplan.Over(tplan.Lag(id.Expr(), offset.Expr(), fallback.Expr()), spec), windowPointers[int64](9, 1, 2, 3, 4, 5), tplan.BindValue(offset, int32(1)), tplan.BindValue(fallback, int64(9)))
		windowCheck(f, "lead", pg.Int8(), tplan.Over(tplan.Lead(id.Expr(), offset.Expr(), tplan.TypedNull(pg.Int8())), spec), []*int64{new(int64(2)), new(int64(3)), new(int64(4)), new(int64(5)), new(int64(6)), nil}, tplan.BindValue(offset, int32(1)))
		windowCheck(f, "first", pg.Int8(), tplan.Over(tplan.FirstValue(id.Expr()), spec), windowPointers[int64](1, 1, 1, 1, 1, 1))
		windowCheck(f, "last", pg.Int8(), tplan.Over(tplan.LastValue(id.Expr()), spec), windowPointers[int64](6, 6, 6, 6, 6, 6))
		windowCheck(f, "nth", pg.Int8(), tplan.Over(tplan.NthValue(id.Expr(), offset.Expr()), spec), windowPointers[int64](2, 2, 2, 2, 2, 2), tplan.BindValue(offset, int32(2)))
		spec.Frame = tplan.WindowFrame{}
		windowCheck(f, "default_last", pg.Int8(), tplan.Over(tplan.LastValue(id.Expr()), spec), windowPointers[int64](1, 2, 3, 4, 5, 6))
		three := tplan.Input("three", pg.Int8())
		spec.Partition = []tplan.Scalar{tplan.Le(id.Expr(), three.Expr())}
		windowCheck(f, "partitioned", pg.Int8(), tplan.Over(tplan.RowNumber(), spec), windowPointers[int64](1, 2, 3, 1, 2, 3), tplan.BindValue(three, int64(3)))
		spec.Partition = nil
		for _, call := range []tplan.Expr[int64]{tplan.Over(tplan.NthValue(id.Expr(), offset.Expr()), spec), tplan.Over(tplan.Lag(id.Expr(), offset.Expr(), tplan.TypedNull(pg.Int8())), spec)} {
			windowCheck(f, "missing", pg.Int8(), call, make([]*int64, 6), tplan.BindValue(offset, int32(9)))
		}
	})
}

func TestWindowFramesAndAggregates(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		id := tplan.FieldOf(f.src.Rel(), "id", pg.Int8())
		distance := tplan.Input("distance", pg.Int8())
		two := tplan.Input("two", pg.Int8())
		peer := tplan.Divide(id.Expr(), two.Expr())
		for _, test := range []struct {
			name string
			spec tplan.WindowSpec
			want []int64
			args []tplan.Binding
		}{
			{"running", tplan.WindowSpec{Order: []tplan.RowTerm{tplan.Asc(id)}, Frame: tplan.RunningRows()}, []int64{1, 2, 3, 4, 5, 6}, nil},
			{"rows", tplan.WindowSpec{Order: []tplan.RowTerm{tplan.Asc(id)}, Frame: tplan.WindowFrame{Mode: tplan.FrameRows, Start: tplan.Preceding(distance.Expr()), End: tplan.CurrentRow()}}, []int64{1, 2, 2, 2, 2, 2}, []tplan.Binding{tplan.BindValue(distance, int64(1))}},
			{"range", tplan.WindowSpec{Order: []tplan.RowTerm{tplan.AscExpr(peer)}, Frame: tplan.WindowFrame{Mode: tplan.FrameRange, Start: tplan.Preceding(distance.Expr()), End: tplan.CurrentRow()}}, []int64{1, 3, 3, 4, 4, 3}, []tplan.Binding{tplan.BindValue(distance, int64(1)), tplan.BindValue(two, int64(2))}},
			{"groups", tplan.WindowSpec{Order: []tplan.RowTerm{tplan.AscExpr(peer)}, Frame: tplan.WindowFrame{Mode: tplan.FrameGroups, Start: tplan.CurrentRow(), End: tplan.Following(distance.Expr())}}, []int64{3, 4, 4, 3, 3, 1}, []tplan.Binding{tplan.BindValue(distance, int64(1)), tplan.BindValue(two, int64(2))}},
		} {
			windowCheck(f, test.name, pg.Int8(), tplan.Over(tplan.CountAll(), test.spec), windowPointers(test.want...), test.args...)
		}
		for exclusion, want := range map[tplan.FrameExclusion][]int64{
			tplan.ExcludeNoOthers: {6, 6, 6, 6, 6, 6}, tplan.ExcludeCurrentRow: {5, 5, 5, 5, 5, 5},
			tplan.ExcludeGroup: {5, 4, 4, 4, 4, 5}, tplan.ExcludeTies: {6, 5, 5, 5, 5, 6},
		} {
			frame := tplan.WholePartition()
			frame.Exclude = exclusion
			windowCheck(f, fmt.Sprint("exclude_", exclusion), pg.Int8(), tplan.Over(tplan.CountAll(), tplan.WindowSpec{Order: []tplan.RowTerm{tplan.AscExpr(peer)}, Frame: frame}), windowPointers(want...), tplan.BindValue(two, int64(2)))
		}
		spec := tplan.WindowSpec{Order: []tplan.RowTerm{tplan.Asc(id)}, Frame: tplan.RunningRows()}
		windowCheck(f, "filtered_count", pg.Int8(), tplan.Over(tplan.FilterWhere(tplan.CountAll(), tplan.Gt(id.Expr(), two.Expr())), spec), windowPointers[int64](0, 0, 1, 2, 3, 4), tplan.BindValue(two, int64(2)))
		windowCheck(f, "sum", pg.Numeric[string](), tplan.Over(tplan.SumOf(id.Expr(), pg.Numeric[string]()), spec), windowPointers("1", "3", "6", "10", "15", "21"))
		windowCheck(f, "avg", pg.Float8(), tplan.Cast(pg.Float8(), tplan.Over(tplan.AvgOf(id.Expr(), pg.Numeric[string]()), spec)), windowPointers[float64](1, 1.5, 2, 2.5, 3, 3.5))
		windowCheck(f, "min", pg.Int8(), tplan.Over(tplan.MinOf(id.Expr()), spec), windowPointers[int64](1, 1, 1, 1, 1, 1))
		windowCheck(f, "max", pg.Int8(), tplan.Over(tplan.MaxOf(id.Expr()), spec), windowPointers[int64](1, 2, 3, 4, 5, 6))
		windowCheck(f, "bool_or", pg.Bool(), tplan.Over(tplan.BoolOr(tplan.Gt(id.Expr(), two.Expr())), spec), windowPointers(false, false, true, true, true, true), tplan.BindValue(two, int64(2)))
		windowCheck(f, "bool_and", pg.Bool(), tplan.Over(tplan.BoolAnd(tplan.Le(id.Expr(), two.Expr())), spec), windowPointers(true, true, false, false, false, false), tplan.BindValue(two, int64(2)))
		separator := tplan.Input("separator", pg.Text())
		windowCheck(f, "string", pg.Text(), tplan.Over(tplan.StringAgg(tplan.Cast(pg.Text(), id.Expr()), separator.Expr()), spec), windowPointers("1", "1:2", "1:2:3", "1:2:3:4", "1:2:3:4:5", "1:2:3:4:5:6"), tplan.BindValue(separator, ":"))
		windowCheck(f, "array", pg.Text(), tplan.Cast(pg.Text(), tplan.Over(tplan.ArrayAgg(id.Expr()), spec)), windowPointers("{1}", "{1,2}", "{1,2,3}", "{1,2,3,4}", "{1,2,3,4,5}", "{1,2,3,4,5,6}"))
		json := windowPointers("[1]", "[1, 2]", "[1, 2, 3]", "[1, 2, 3, 4]", "[1, 2, 3, 4, 5]", "[1, 2, 3, 4, 5, 6]")
		windowCheck(f, "json", pg.Text(), tplan.Cast(pg.Text(), tplan.Over(tplan.JSONAgg(pg.JSON[[]int64](), id.Expr()), spec)), json)
		windowCheck(f, "jsonb", pg.Text(), tplan.Cast(pg.Text(), tplan.Over(tplan.JSONBAgg(pg.JSONB[[]int64](), id.Expr()), spec)), json)
		empty := tplan.WindowFrame{Mode: tplan.FrameRows, Start: tplan.CurrentRow(), End: tplan.CurrentRow(), Exclude: tplan.ExcludeCurrentRow}
		windowCheck(f, "empty_frame", pg.Int8(), tplan.Over(tplan.FirstValue(id.Expr()), tplan.WindowSpec{Frame: empty}), make([]*int64, 6))
		bad := windowPlan(f, pg.Int8(), tplan.Over(tplan.CountAll(), tplan.WindowSpec{Order: []tplan.RowTerm{tplan.Asc(id)}, Frame: tplan.WindowFrame{Mode: tplan.FrameRows, Start: tplan.Preceding(distance.Expr()), End: tplan.CurrentRow()}}))
		err := f.run(f.t.Context(), func(ctx context.Context, s session) error {
			_, err := bad.Bind(s).Find(ctx, tplan.BindValue(distance, int64(-1)))
			return err
		})
		require.Equal(f.t, "22013", q.SQLState(err))
	})
}

func TestWindowAfterGroupedAggregation(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		t := f.t
		id := tplan.FieldOf(f.src.Rel(), "id", pg.Int8())
		two := tplan.Input("two", pg.Int8())
		group := tplan.Divide(id.Expr(), two.Expr())
		value := tplan.Over(tplan.SumOf(tplan.CountAll(), pg.Numeric[string]()), tplan.WindowSpec{Order: []tplan.RowTerm{tplan.AscExpr(group)}, Frame: tplan.RunningRows()})
		out := q.DefineView(f.reg, q.ViewSpec[windowValue[string]]{Name: "groups"})
		plan := tplan.DefineRead(out, "totals", tplan.Select(tplan.Project(tplan.Output(out, "id", pg.Int8()), group), tplan.Project(tplan.Output(out, "value", pg.Numeric[string]()), value)).From(f.src).GroupBy(group).OrderBy(tplan.AscExpr(group)))
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			rows, err := plan.Bind(s).Find(ctx, tplan.BindValue(two, int64(2)))
			require.NoError(t, err)
			require.Equal(t, []windowValue[string]{{ID: 0, Value: new("1")}, {ID: 1, Value: new("3")}, {ID: 2, Value: new("5")}, {ID: 3, Value: new("6")}}, rows)
			return nil
		}))
	})
}
