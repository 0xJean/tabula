//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/stretchr/testify/require"
)

func TestWindowPaginationAndCount(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		t := f.t
		id := tplan.FieldOf(f.src.Rel(), "id", pg.Int8())
		position := tplan.Over(tplan.RowNumber(), tplan.WindowSpec{Order: []tplan.RowTerm{tplan.Asc(id)}})
		out := q.DefineView(f.reg, q.ViewSpec[windowValue[int64]]{Name: "positions"})
		project := []tplan.Projection{tplan.Project(tplan.Output(out, "id", pg.Int8()), id.Expr()), tplan.Project(tplan.Output(out, "value", pg.Int8()), position)}
		query := tplan.Select(project...).From(f.src).WithList(tplan.ReadListSpec{
			Orders: []tplan.RowOrder{{Name: "id", Terms: []tplan.RowTerm{tplan.Asc(id)}}}, Pages: tplan.RowPages{Keyset: true, Offset: true, MaxLimit: 3, MaxOffset: 6}, ExactCount: true,
		})
		plan := tplan.DefineRead(out, "positions", query)
		stmt := out.Definition().Statements["positions.global.list.data.sort.id.after"]
		require.Contains(t, stmt, `) AS "result" WHERE `)
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			bound := plan.Bind(s)
			first, count, err := bound.List(ctx, tplan.RowChoice{Order: "id", Page: tplan.FirstPage, Limit: 2})
			require.NoError(t, err)
			require.EqualValues(t, 6, count)
			require.Equal(t, []windowValue[int64]{{ID: 1, Value: new(int64(1))}, {ID: 2, Value: new(int64(2))}}, first)
			cursor, err := plan.CursorFrom("id", first[1])
			require.NoError(t, err)
			after, count, err := bound.List(ctx, tplan.RowChoice{Order: "id", Page: tplan.AfterPage, Limit: 2, After: cursor})
			require.NoError(t, err)
			require.EqualValues(t, 6, count)
			require.Equal(t, []windowValue[int64]{{ID: 3, Value: new(int64(3))}, {ID: 4, Value: new(int64(4))}}, after)
			return nil
		}))
		rows := tplan.DefineRows(out, "rows", f.src, tplan.RowsSpec{Orders: []tplan.RowOrder{{Name: "id", Terms: []tplan.RowTerm{tplan.Asc(id)}}}, Pages: tplan.RowPages{Keyset: true, MaxLimit: 3}}, project...)
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			cursor, err := rows.Cursor("id", tplan.At(id, int64(2)))
			require.NoError(t, err)
			got, err := rows.Bind(s).Find(ctx, tplan.RowChoice{Order: "id", Page: tplan.AfterPage, Limit: 2, After: cursor})
			require.NoError(t, err)
			require.Equal(t, []windowValue[int64]{{ID: 3, Value: new(int64(3))}, {ID: 4, Value: new(int64(4))}}, got)
			return nil
		}))
	})
}

func TestWindowRangeIntervalAndNulls(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		id := tplan.FieldOf(f.src.Rel(), "id", pg.Int8())
		three := tplan.Input("three", pg.Int8())
		value := tplan.Case(tplan.When(tplan.Eq(id.Expr(), three.Expr()), tplan.TypedNull(pg.Int8()))).Else(id.Expr())
		windowCheck(f, "nulls_first", pg.Int8(), tplan.Over(tplan.Rank(), tplan.WindowSpec{Order: []tplan.RowTerm{tplan.DescExpr(value).NullsFirst()}}), windowPointers[int64](6, 5, 1, 4, 3, 2), tplan.BindValue(three, int64(3)))
		start := tplan.Input("start", pg.Timestamp())
		span := tplan.Input("span", pg.Interval())
		stamp := tplan.PlusAs(pg.Timestamp(), start.Expr(), tplan.TimesAs(pg.Interval(), span.Expr(), tplan.Cast(pg.Float8(), id.Expr())))
		spec := tplan.WindowSpec{Order: []tplan.RowTerm{tplan.AscExpr(stamp)}, Frame: tplan.WindowFrame{Mode: tplan.FrameRange, Start: tplan.Preceding(span.Expr()), End: tplan.CurrentRow()}}
		windowCheck(f, "interval", pg.Int8(), tplan.Over(tplan.CountAll(), spec), windowPointers[int64](1, 2, 2, 2, 2, 2), tplan.BindValue(start, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)), tplan.BindValue(span, "1 hour"))
	})
}
