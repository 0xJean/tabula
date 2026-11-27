//go:build integration

package integration_test

import (
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
)

func windowRangeNative[A, D any](f *scalarFixture, name string, orderType q.PGType[A], order A, distanceType q.PGType[D], distance D) {
	f.t.Helper()
	a, b := q.Input("order", orderType), q.Input("distance", distanceType)
	spec := q.WindowSpec{Order: []q.RowTerm{q.AscExpr(a.Expr())}, Frame: q.WindowFrame{Mode: q.FrameRange, Start: q.Preceding(b.Expr()), End: q.CurrentRow()}}
	windowCheck(f, name, pg.Int8(), q.Over(q.CountAll(), spec), windowPointers[int64](6, 6, 6, 6, 6, 6), q.BindValue(a, order), q.BindValue(b, distance))
}

func TestWindowRangeNativeOverloads(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		windowRangeNative(f, "int2_int2", pg.Int2(), int16(2), pg.Int2(), int16(1))
		windowRangeNative(f, "int2_int4", pg.Int2(), int16(2), pg.Int4(), int32(1))
		windowRangeNative(f, "int2_int8", pg.Int2(), int16(2), pg.Int8(), int64(1))
		windowRangeNative(f, "int4_int2", pg.Int4(), int32(2), pg.Int2(), int16(1))
		windowRangeNative(f, "int4_int4", pg.Int4(), int32(2), pg.Int4(), int32(1))
		windowRangeNative(f, "int4_int8", pg.Int4(), int32(2), pg.Int8(), int64(1))
		windowRangeNative(f, "int8", pg.Int8(), int64(2), pg.Int8(), int64(1))
		windowRangeNative(f, "numeric", pg.Numeric[string](), "2", pg.Numeric[string](), "1")
		windowRangeNative(f, "float4", pg.Float4(), float32(2), pg.Float8(), float64(1))
		windowRangeNative(f, "float8", pg.Float8(), float64(2), pg.Float8(), float64(1))
		day := time.Date(2020, 2, 3, 0, 0, 0, 0, time.UTC)
		windowRangeNative(f, "date", pg.Date(), day, pg.Interval(), "1 day")
		windowRangeNative(f, "time", pg.Time(), "12:00:00", pg.Interval(), "1 hour")
		windowRangeNative(f, "timetz", pg.Timetz(), "12:00:00+00", pg.Interval(), "1 hour")
		windowRangeNative(f, "timestamp", pg.Timestamp(), day, pg.Interval(), "1 day")
		windowRangeNative(f, "timestamptz", pg.Timestamptz(), day, pg.Interval(), "1 day")
		windowRangeNative(f, "interval", pg.Interval(), "2 hours", pg.Interval(), "1 hour")
	})
}
