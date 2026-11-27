//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/stretchr/testify/require"
)

type windowValue[V any] struct {
	ID    int64 `db:"id"`
	Value *V    `db:"value"`
}

func windowPlan[V any](f *scalarFixture, kind q.PGType[V], value q.Expr[V]) *q.ReadPlan[windowValue[V], struct{}] {
	f.t.Helper()
	f.serial++
	out := q.DefineView(f.reg, q.ViewSpec[windowValue[V]]{Name: fmt.Sprintf("entries_%d", f.serial)})
	id := q.FieldOf(f.src.Rel(), "id", pg.Int8())
	plan := q.DefineRead(out, "windowed", q.Select(q.Project(q.Output(out, "id", pg.Int8()), id.Expr()), q.Project(q.Output(out, "value", kind), value)).From(f.src).OrderBy(q.Asc(id)))
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

func windowCheck[V any](f *scalarFixture, name string, kind q.PGType[V], value q.Expr[V], want []*V, args ...q.Binding) {
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
		id := q.FieldOf(f.src.Rel(), "id", pg.Int8())
		two := q.Input("two", pg.Int8())
		offset, fallback := q.Input("offset", pg.Int4()), q.Input("fallback", pg.Int8())
		spec := q.WindowSpec{Order: []q.RowTerm{q.AscExpr(q.Divide(id.Expr(), two.Expr()))}}
		arg := q.BindValue(two, int64(2))
		windowCheck(f, "row_number", pg.Int8(), q.Over(q.RowNumber(), q.WindowSpec{Order: []q.RowTerm{q.Asc(id)}}), windowPointers[int64](1, 2, 3, 4, 5, 6))
		windowCheck(f, "rank", pg.Int8(), q.Over(q.Rank(), spec), windowPointers[int64](1, 2, 2, 4, 4, 6), arg)
		windowCheck(f, "dense_rank", pg.Int8(), q.Over(q.DenseRank(), spec), windowPointers[int64](1, 2, 2, 3, 3, 4), arg)
		windowCheck(f, "percent_rank", pg.Float8(), q.Over(q.PercentRank(), spec), windowPointers[float64](0, .2, .2, .6, .6, 1), arg)
		windowCheck(f, "cume_dist", pg.Float8(), q.Over(q.CumeDist(), spec), windowPointers[float64](1.0/6, .5, .5, 5.0/6, 5.0/6, 1), arg)
		windowCheck(f, "ntile", pg.Int4(), q.Over(q.NTile(offset.Expr()), q.WindowSpec{Order: []q.RowTerm{q.Asc(id)}}), windowPointers[int32](1, 1, 1, 2, 2, 2), q.BindValue(offset, int32(2)))
		spec = q.WindowSpec{Name: "values", Order: []q.RowTerm{q.Asc(id)}, Frame: q.WholePartition()}
		windowCheck(f, "lag", pg.Int8(), q.Over(q.Lag(id.Expr(), offset.Expr(), fallback.Expr()), spec), windowPointers[int64](9, 1, 2, 3, 4, 5), q.BindValue(offset, int32(1)), q.BindValue(fallback, int64(9)))
		windowCheck(f, "lead", pg.Int8(), q.Over(q.Lead(id.Expr(), offset.Expr(), q.TypedNull(pg.Int8())), spec), []*int64{new(int64(2)), new(int64(3)), new(int64(4)), new(int64(5)), new(int64(6)), nil}, q.BindValue(offset, int32(1)))
		windowCheck(f, "first", pg.Int8(), q.Over(q.FirstValue(id.Expr()), spec), windowPointers[int64](1, 1, 1, 1, 1, 1))
		windowCheck(f, "last", pg.Int8(), q.Over(q.LastValue(id.Expr()), spec), windowPointers[int64](6, 6, 6, 6, 6, 6))
		windowCheck(f, "nth", pg.Int8(), q.Over(q.NthValue(id.Expr(), offset.Expr()), spec), windowPointers[int64](2, 2, 2, 2, 2, 2), q.BindValue(offset, int32(2)))
		spec.Frame = q.WindowFrame{}
		windowCheck(f, "default_last", pg.Int8(), q.Over(q.LastValue(id.Expr()), spec), windowPointers[int64](1, 2, 3, 4, 5, 6))
		three := q.Input("three", pg.Int8())
		spec.Partition = []q.Scalar{q.Le(id.Expr(), three.Expr())}
		windowCheck(f, "partitioned", pg.Int8(), q.Over(q.RowNumber(), spec), windowPointers[int64](1, 2, 3, 1, 2, 3), q.BindValue(three, int64(3)))
		spec.Partition = nil
		for _, call := range []q.Expr[int64]{q.Over(q.NthValue(id.Expr(), offset.Expr()), spec), q.Over(q.Lag(id.Expr(), offset.Expr(), q.TypedNull(pg.Int8())), spec)} {
			windowCheck(f, "missing", pg.Int8(), call, make([]*int64, 6), q.BindValue(offset, int32(9)))
		}
	})
}

func TestWindowFramesAndAggregates(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		id := q.FieldOf(f.src.Rel(), "id", pg.Int8())
		distance := q.Input("distance", pg.Int8())
		two := q.Input("two", pg.Int8())
		peer := q.Divide(id.Expr(), two.Expr())
		for _, test := range []struct {
			name string
			spec q.WindowSpec
			want []int64
			args []q.Binding
		}{
			{"running", q.WindowSpec{Order: []q.RowTerm{q.Asc(id)}, Frame: q.RunningRows()}, []int64{1, 2, 3, 4, 5, 6}, nil},
			{"rows", q.WindowSpec{Order: []q.RowTerm{q.Asc(id)}, Frame: q.WindowFrame{Mode: q.FrameRows, Start: q.Preceding(distance.Expr()), End: q.CurrentRow()}}, []int64{1, 2, 2, 2, 2, 2}, []q.Binding{q.BindValue(distance, int64(1))}},
			{"range", q.WindowSpec{Order: []q.RowTerm{q.AscExpr(peer)}, Frame: q.WindowFrame{Mode: q.FrameRange, Start: q.Preceding(distance.Expr()), End: q.CurrentRow()}}, []int64{1, 3, 3, 4, 4, 3}, []q.Binding{q.BindValue(distance, int64(1)), q.BindValue(two, int64(2))}},
			{"groups", q.WindowSpec{Order: []q.RowTerm{q.AscExpr(peer)}, Frame: q.WindowFrame{Mode: q.FrameGroups, Start: q.CurrentRow(), End: q.Following(distance.Expr())}}, []int64{3, 4, 4, 3, 3, 1}, []q.Binding{q.BindValue(distance, int64(1)), q.BindValue(two, int64(2))}},
		} {
			windowCheck(f, test.name, pg.Int8(), q.Over(q.CountAll(), test.spec), windowPointers(test.want...), test.args...)
		}
		for exclusion, want := range map[q.FrameExclusion][]int64{
			q.ExcludeNoOthers: {6, 6, 6, 6, 6, 6}, q.ExcludeCurrentRow: {5, 5, 5, 5, 5, 5},
			q.ExcludeGroup: {5, 4, 4, 4, 4, 5}, q.ExcludeTies: {6, 5, 5, 5, 5, 6},
		} {
			frame := q.WholePartition()
			frame.Exclude = exclusion
			windowCheck(f, fmt.Sprint("exclude_", exclusion), pg.Int8(), q.Over(q.CountAll(), q.WindowSpec{Order: []q.RowTerm{q.AscExpr(peer)}, Frame: frame}), windowPointers(want...), q.BindValue(two, int64(2)))
		}
		spec := q.WindowSpec{Order: []q.RowTerm{q.Asc(id)}, Frame: q.RunningRows()}
		windowCheck(f, "filtered_count", pg.Int8(), q.Over(q.FilterWhere(q.CountAll(), q.Gt(id.Expr(), two.Expr())), spec), windowPointers[int64](0, 0, 1, 2, 3, 4), q.BindValue(two, int64(2)))
		windowCheck(f, "sum", pg.Numeric[string](), q.Over(q.SumOf(id.Expr(), pg.Numeric[string]()), spec), windowPointers("1", "3", "6", "10", "15", "21"))
		windowCheck(f, "avg", pg.Float8(), q.Cast(pg.Float8(), q.Over(q.AvgOf(id.Expr(), pg.Numeric[string]()), spec)), windowPointers[float64](1, 1.5, 2, 2.5, 3, 3.5))
		windowCheck(f, "min", pg.Int8(), q.Over(q.MinOf(id.Expr()), spec), windowPointers[int64](1, 1, 1, 1, 1, 1))
		windowCheck(f, "max", pg.Int8(), q.Over(q.MaxOf(id.Expr()), spec), windowPointers[int64](1, 2, 3, 4, 5, 6))
		windowCheck(f, "bool_or", pg.Bool(), q.Over(q.BoolOr(q.Gt(id.Expr(), two.Expr())), spec), windowPointers(false, false, true, true, true, true), q.BindValue(two, int64(2)))
		windowCheck(f, "bool_and", pg.Bool(), q.Over(q.BoolAnd(q.Le(id.Expr(), two.Expr())), spec), windowPointers(true, true, false, false, false, false), q.BindValue(two, int64(2)))
		separator := q.Input("separator", pg.Text())
		windowCheck(f, "string", pg.Text(), q.Over(q.StringAgg(q.Cast(pg.Text(), id.Expr()), separator.Expr()), spec), windowPointers("1", "1:2", "1:2:3", "1:2:3:4", "1:2:3:4:5", "1:2:3:4:5:6"), q.BindValue(separator, ":"))
		windowCheck(f, "array", pg.Text(), q.Cast(pg.Text(), q.Over(q.ArrayAgg(id.Expr()), spec)), windowPointers("{1}", "{1,2}", "{1,2,3}", "{1,2,3,4}", "{1,2,3,4,5}", "{1,2,3,4,5,6}"))
		json := windowPointers("[1]", "[1, 2]", "[1, 2, 3]", "[1, 2, 3, 4]", "[1, 2, 3, 4, 5]", "[1, 2, 3, 4, 5, 6]")
		windowCheck(f, "json", pg.Text(), q.Cast(pg.Text(), q.Over(q.JSONAgg(pg.JSON[[]int64](), id.Expr()), spec)), json)
		windowCheck(f, "jsonb", pg.Text(), q.Cast(pg.Text(), q.Over(q.JSONBAgg(pg.JSONB[[]int64](), id.Expr()), spec)), json)
		empty := q.WindowFrame{Mode: q.FrameRows, Start: q.CurrentRow(), End: q.CurrentRow(), Exclude: q.ExcludeCurrentRow}
		windowCheck(f, "empty_frame", pg.Int8(), q.Over(q.FirstValue(id.Expr()), q.WindowSpec{Frame: empty}), make([]*int64, 6))
		bad := windowPlan(f, pg.Int8(), q.Over(q.CountAll(), q.WindowSpec{Order: []q.RowTerm{q.Asc(id)}, Frame: q.WindowFrame{Mode: q.FrameRows, Start: q.Preceding(distance.Expr()), End: q.CurrentRow()}}))
		err := f.run(f.t.Context(), func(ctx context.Context, s session) error {
			_, err := bad.Bind(s).Find(ctx, q.BindValue(distance, int64(-1)))
			return err
		})
		require.Equal(f.t, "22013", q.SQLState(err))
	})
}

func TestWindowAfterGroupedAggregation(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		t := f.t
		id := q.FieldOf(f.src.Rel(), "id", pg.Int8())
		two := q.Input("two", pg.Int8())
		group := q.Divide(id.Expr(), two.Expr())
		value := q.Over(q.SumOf(q.CountAll(), pg.Numeric[string]()), q.WindowSpec{Order: []q.RowTerm{q.AscExpr(group)}, Frame: q.RunningRows()})
		out := q.DefineView(f.reg, q.ViewSpec[windowValue[string]]{Name: "groups"})
		plan := q.DefineRead(out, "totals", q.Select(q.Project(q.Output(out, "id", pg.Int8()), group), q.Project(q.Output(out, "value", pg.Numeric[string]()), value)).From(f.src).GroupBy(group).OrderBy(q.AscExpr(group)))
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			rows, err := plan.Bind(s).Find(ctx, q.BindValue(two, int64(2)))
			require.NoError(t, err)
			require.Equal(t, []windowValue[string]{{ID: 0, Value: new("1")}, {ID: 1, Value: new("3")}, {ID: 2, Value: new("5")}, {ID: 3, Value: new("6")}}, rows)
			return nil
		}))
	})
}
