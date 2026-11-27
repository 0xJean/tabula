//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/stretchr/testify/require"
)

func TestWindowPaginationAndCount(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		t := f.t
		id := q.FieldOf(f.src.Rel(), "id", pg.Int8())
		position := q.Over(q.RowNumber(), q.WindowSpec{Order: []q.RowTerm{q.Asc(id)}})
		out := q.DefineView(f.reg, q.ViewSpec[windowValue[int64]]{Name: "positions"})
		project := []q.Projection{q.Project(q.Output(out, "id", pg.Int8()), id.Expr()), q.Project(q.Output(out, "value", pg.Int8()), position)}
		query := q.Select(project...).From(f.src).WithList(q.ReadListSpec{
			Orders: []q.RowOrder{{Name: "id", Terms: []q.RowTerm{q.Asc(id)}}}, Pages: q.RowPages{Keyset: true, Offset: true, MaxLimit: 3, MaxOffset: 6}, ExactCount: true,
		})
		plan := q.DefineRead(out, "positions", query)
		stmt := out.Definition().Statements["positions.global.list.data.sort.id.after"]
		require.Contains(t, stmt, `) AS "result" WHERE `)
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			bound := plan.Bind(s)
			first, count, err := bound.List(ctx, q.RowChoice{Order: "id", Page: q.FirstPage, Limit: 2})
			require.NoError(t, err)
			require.EqualValues(t, 6, count)
			require.Equal(t, []windowValue[int64]{{ID: 1, Value: new(int64(1))}, {ID: 2, Value: new(int64(2))}}, first)
			cursor, err := plan.CursorFrom("id", first[1])
			require.NoError(t, err)
			after, count, err := bound.List(ctx, q.RowChoice{Order: "id", Page: q.AfterPage, Limit: 2, After: cursor})
			require.NoError(t, err)
			require.EqualValues(t, 6, count)
			require.Equal(t, []windowValue[int64]{{ID: 3, Value: new(int64(3))}, {ID: 4, Value: new(int64(4))}}, after)
			return nil
		}))
		rows := q.DefineRows(out, "rows", f.src, q.RowsSpec{Orders: []q.RowOrder{{Name: "id", Terms: []q.RowTerm{q.Asc(id)}}}, Pages: q.RowPages{Keyset: true, MaxLimit: 3}}, project...)
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			cursor, err := rows.Cursor("id", q.At(id, int64(2)))
			require.NoError(t, err)
			got, err := rows.Bind(s).Find(ctx, q.RowChoice{Order: "id", Page: q.AfterPage, Limit: 2, After: cursor})
			require.NoError(t, err)
			require.Equal(t, []windowValue[int64]{{ID: 3, Value: new(int64(3))}, {ID: 4, Value: new(int64(4))}}, got)
			return nil
		}))
	})
}

func TestWindowRangeIntervalAndNulls(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		id := q.FieldOf(f.src.Rel(), "id", pg.Int8())
		three := q.Input("three", pg.Int8())
		value := q.Case(q.When(q.Eq(id.Expr(), three.Expr()), q.TypedNull(pg.Int8()))).Else(id.Expr())
		windowCheck(f, "nulls_first", pg.Int8(), q.Over(q.Rank(), q.WindowSpec{Order: []q.RowTerm{q.DescExpr(value).NullsFirst()}}), windowPointers[int64](6, 5, 1, 4, 3, 2), q.BindValue(three, int64(3)))
		start := q.Input("start", pg.Timestamp())
		span := q.Input("span", pg.Interval())
		stamp := q.PlusAs(pg.Timestamp(), start.Expr(), q.TimesAs(pg.Interval(), span.Expr(), q.Cast(pg.Float8(), id.Expr())))
		spec := q.WindowSpec{Order: []q.RowTerm{q.AscExpr(stamp)}, Frame: q.WindowFrame{Mode: q.FrameRange, Start: q.Preceding(span.Expr()), End: q.CurrentRow()}}
		windowCheck(f, "interval", pg.Int8(), q.Over(q.CountAll(), spec), windowPointers[int64](1, 2, 2, 2, 2, 2), q.BindValue(start, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)), q.BindValue(span, "1 hour"))
	})
}
