//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/stretchr/testify/require"
)

func TestScalarJSONAndArrays(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		doc := q.Input("doc", pg.JSONB[json.RawMessage]())
		key, path, index := q.Input("key", pg.Text()), q.Input("path", pg.ArrayOf(pg.Text())), q.Input("index", pg.Int4())
		value := q.BindValue(doc, json.RawMessage(`{"a":[1,null,3],"b":null}`))
		scalarCheck(f, "get", pg.Text(), q.Cast(pg.Text(), q.JSONGet(doc.Expr(), key.Expr())), new("[1, null, 3]"), value, q.BindValue(key, "a"))
		scalarCheck(f, "missing", pg.Text(), q.JSONGetText(doc.Expr(), key.Expr()), nil, value, q.BindValue(key, "missing"))
		scalarCheck(f, "json_null", pg.Text(), q.Cast(pg.Text(), q.JSONGet(doc.Expr(), key.Expr())), new("null"), value, q.BindValue(key, "b"))
		scalarCheck(f, "json_null_text", pg.Text(), q.JSONGetText(doc.Expr(), key.Expr()), nil, value, q.BindValue(key, "b"))
		scalarCheck(f, "path", pg.Text(), q.JSONGetTextPath(doc.Expr(), path.Expr()), new("3"), value, q.BindValue(path, []string{"a", "-1"}))
		scalarCheck(f, "path_json", pg.Text(), q.Cast(pg.Text(), q.JSONGetPath(doc.Expr(), path.Expr())), new("null"), value, q.BindValue(path, []string{"a", "1"}))
		arrayDoc := q.BindValue(doc, json.RawMessage(`[1,2,3]`))
		scalarCheck(f, "at", pg.Text(), q.Cast(pg.Text(), q.JSONGetAt(doc.Expr(), index.Expr())), new("3"), arrayDoc, q.BindValue(index, int32(-1)))
		scalarCheck(f, "at_text", pg.Text(), q.JSONGetTextAt(doc.Expr(), index.Expr()), new("2"), arrayDoc, q.BindValue(index, int32(1)))
		scalarCheck(f, "has", pg.Bool(), q.JSONHasKey(doc.Expr(), key.Expr()), new(true), value, q.BindValue(key, "b"))
		scalarCheck(f, "contains", pg.Bool(), q.JSONContains(doc.Expr(), doc.Expr()), new(true), value)
		scalarCheck(f, "contained", pg.Bool(), q.JSONContainedBy(doc.Expr(), doc.Expr()), new(true), value)
		scalarCheck(f, "type", pg.Text(), q.JSONTypeOf(doc.Expr()), new("object"), value)
		scalarCheck(f, "cat", pg.Text(), q.Cast(pg.Text(), q.JSONBCat(doc.Expr(), doc.Expr())), new("[1, 2, 3, 1, 2, 3]"), arrayDoc)
		scalarCheck(f, "delete_key", pg.Text(), q.Cast(pg.Text(), q.JSONBDeleteKey(doc.Expr(), key.Expr())), new(`{"b": null}`), value, q.BindValue(key, "a"))
		scalarCheck(f, "delete_keys", pg.Text(), q.Cast(pg.Text(), q.JSONBDeleteKeys(doc.Expr(), path.Expr())), new(`{}`), value, q.BindValue(path, []string{"a", "b"}))
		scalarCheck(f, "delete_at", pg.Text(), q.Cast(pg.Text(), q.JSONBDeleteAt(doc.Expr(), index.Expr())), new(`[1, 2]`), arrayDoc, q.BindValue(index, int32(-1)))
		scalarCheck(f, "delete_path", pg.Text(), q.Cast(pg.Text(), q.JSONBDeletePath(doc.Expr(), path.Expr())), new(`{"a": [1, 3], "b": null}`), value, q.BindValue(path, []string{"a", "1"}))
		first, second := q.Input("first", pg.Int4()), q.Input("second", pg.Int4())
		pairs := []q.JSONMember{q.JSONPair(key.Expr(), first.Expr()), q.JSONPair(key.Expr(), second.Expr())}
		args := []q.Binding{q.BindValue(key, "x"), q.BindValue(first, int32(1)), q.BindValue(second, int32(2))}
		scalarCheck(f, "build_json", pg.Text(), q.Cast(pg.Text(), q.JSONBuildObject(pg.JSON[json.RawMessage](), pairs...)), new(`{"x" : 1, "x" : 2}`), args...)
		scalarCheck(f, "build_jsonb", pg.Text(), q.Cast(pg.Text(), q.JSONBuildObject(pg.JSONB[json.RawMessage](), pairs...)), new(`{"x": 2}`), args...)
		scalarCheck(f, "build_empty", pg.Text(), q.Cast(pg.Text(), q.JSONBuildObject(pg.JSONB[json.RawMessage]())), new(`{}`))
		scalarCheck(f, "build_null_value", pg.Text(), q.Cast(pg.Text(), q.JSONBuildObject(pg.JSONB[json.RawMessage](), q.JSONPair(key.Expr(), q.TypedNull(pg.Int4())))), new(`{"x": null}`), q.BindValue(key, "x"))
		arr := q.Input("array", pg.ArrayOf(pg.Int4()))
		arrayBinding := q.BindValue(arr, []int32{1, 2, 1})
		scalarCheck(f, "array_expr", pg.Text(), q.Cast(pg.Text(), q.ArrayExpr(first.Expr(), q.TypedNull(pg.Int4()), first.Expr())), new(`{1,NULL,1}`), q.BindValue(first, int32(1)))
		scalarCheck(f, "array_cat", pg.Text(), q.Cast(pg.Text(), q.ArrayCat(arr.Expr(), q.TypedNull(pg.ArrayOf(pg.Int4())))), new(`{1,2,1}`), arrayBinding)
		scalarCheck(f, "array_contains", pg.Bool(), q.ArrayContains(arr.Expr(), q.ArrayExpr(first.Expr())), new(true), arrayBinding, q.BindValue(first, int32(1)))
		scalarCheck(f, "array_contained", pg.Bool(), q.ArrayContainedBy(q.ArrayExpr(first.Expr()), arr.Expr()), new(true), arrayBinding, q.BindValue(first, int32(1)))
		scalarCheck(f, "array_overlap", pg.Bool(), q.ArrayOverlaps(arr.Expr(), q.ArrayExpr(first.Expr())), new(false), arrayBinding, q.BindValue(first, int32(3)))
		scalarCheck(f, "cardinality", pg.Int4(), q.Cardinality(arr.Expr()), new(int32(3)), arrayBinding)
		scalarCheck(f, "empty_cardinality", pg.Int4(), q.Cardinality(arr.Expr()), new(int32(0)), q.BindValue(arr, []int32{}))
		scalarCheck(f, "null_cardinality", pg.Int4(), q.Cardinality(arr.Expr()), nil, q.BindValue(arr, []int32(nil)))
		scalarCheck(f, "remove", pg.Text(), q.Cast(pg.Text(), q.ArrayRemove(arr.Expr(), first.Expr())), new(`{2}`), arrayBinding, q.BindValue(first, int32(1)))
		scalarCheck(f, "remove_null", pg.Text(), q.Cast(pg.Text(), q.ArrayRemove(q.ArrayExpr(first.Expr(), q.TypedNull(pg.Int4())), q.TypedNull(pg.Int4()))), new(`{1}`), q.BindValue(first, int32(1)))
		for _, c := range []struct {
			name string
			expr q.Expr[[]int32]
			want *bool
			args []q.Binding
		}{
			{"all_empty", arr.Expr(), new(true), []q.Binding{q.BindValue(arr, []int32{})}},
			{"all_null", arr.Expr(), nil, []q.Binding{q.BindValue(arr, []int32(nil))}},
			{"all_match", q.ArrayExpr(first.Expr(), q.TypedNull(pg.Int4())), new(false), nil},
			{"all_unknown", q.ArrayExpr(q.TypedNull(pg.Int4())), nil, nil},
		} {
			scalarCheck(f, c.name, pg.Bool(), q.NotEqualAll(first.Expr(), c.expr), c.want, append(c.args, q.BindValue(first, int32(1)))...)
		}
	})
}

func TestScalarTimeAndUUID(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		baseRun := f.run
		f.run = func(ctx context.Context, body callback) error {
			return baseRun(ctx, func(ctx context.Context, s session) error {
				_, err := s.Exec(ctx, "SET LOCAL TIME ZONE 'Asia/Kathmandu'")
				if err != nil {
					return err
				}
				return body(ctx, s)
			})
		}
		day := q.Input("day", pg.Date())
		stamp := q.Input("stamp", pg.Timestamptz())
		format, zone := q.Input("format", pg.Text()), q.Input("zone", pg.Text())
		date := time.Date(2024, 3, 10, 0, 0, 0, 0, time.UTC)
		scalarCheck(f, "date_format", pg.Text(), q.ToChar(day.Expr(), format.Expr()), new("2024-03-10 +0545 +05:45"), q.BindValue(day, date), q.BindValue(format, "YYYY-MM-DD TZ OF"))
		scalarCheck(f, "timestamp_format", pg.Text(), q.ToChar(q.Cast(pg.Timestamp(), day.Expr()), format.Expr()), new("2024-03-10  +00"), q.BindValue(day, date), q.BindValue(format, "YYYY-MM-DD TZ OF"))
		scalarCheck(f, "truncate", pg.Text(), q.ToChar(q.DateTrunc(q.Day, stamp.Expr()), format.Expr()), new("2024-03-10 00:00"), q.BindValue(stamp, date), q.BindValue(format, "YYYY-MM-DD HH24:MI"))
		scalarCheck(f, "truncate_zone", pg.Text(), q.ToChar(q.DateTruncAt(q.Day, stamp.Expr(), zone.Expr()), format.Expr()), new("2024-03-09 10:45"), q.BindValue(stamp, date), q.BindValue(zone, "America/New_York"), q.BindValue(format, "YYYY-MM-DD HH24:MI"))
		for _, c := range []struct {
			name string
			day  int
			want string
		}{
			{"zone_before_dst", 10, "2024-03-10 10:45"},
			{"zone_after_dst", 11, "2024-03-11 09:45"},
		} {
			scalarCheck(f, c.name, pg.Text(), q.ToChar(q.DateTruncAt(q.Day, stamp.Expr(), zone.Expr()), format.Expr()), new(c.want),
				q.BindValue(stamp, time.Date(2024, 3, c.day, 12, 0, 0, 0, time.UTC)), q.BindValue(zone, "America/New_York"), q.BindValue(format, "YYYY-MM-DD HH24:MI"))
		}
		scalarCheck(f, "timezone", pg.Text(), q.ToChar(q.Timezone(zone.Expr(), stamp.Expr()), format.Expr()), new("2024-03-09 19:00"), q.BindValue(stamp, date), q.BindValue(zone, "America/New_York"), q.BindValue(format, "YYYY-MM-DD HH24:MI"))
		scalarCheck(f, "extract", pg.Numeric[string](), q.Extract(pg.Numeric[string](), q.Year, stamp.Expr()), new("2024"), q.BindValue(stamp, date))
		span := q.Input("span", pg.Interval())
		scalarCheck(f, "truncate_interval", pg.Text(), q.Cast(pg.Text(), q.DateTrunc(q.Hour, span.Expr())), new("1 day 02:00:00"), q.BindValue(span, "1 day 02:35:42"))
		scalarCheck(f, "extract_interval", pg.Numeric[string](), q.Extract(pg.Numeric[string](), q.Hour, span.Expr()), new("2"), q.BindValue(span, "1 day 02:35:42"))
		zoned := q.Input("zoned", pg.Timetz())
		scalarCheck(f, "timezone_timetz", pg.Text(), q.Cast(pg.Text(), q.Timezone(zone.Expr(), zoned.Expr())), new("10:00:00+00"), q.BindValue(zone, "UTC"), q.BindValue(zoned, "12:00:00+02"))
		scalarCheck(f, "civil_timezone", pg.Text(), q.ToChar(q.Timezone(zone.Expr(), q.Cast(pg.Timestamp(), day.Expr())), format.Expr()), new("2024-03-10 05:45"), q.BindValue(day, date), q.BindValue(zone, "UTC"), q.BindValue(format, "YYYY-MM-DD HH24:MI"))
		scalarCheck(f, "date_plus_integer", pg.Text(), q.ToChar(q.PlusAs(pg.Date(), day.Expr(), q.Cast(pg.Int4(), q.Extract(pg.Numeric[string](), q.Month, day.Expr()))), format.Expr()), new("2024-03-13"), q.BindValue(day, date), q.BindValue(format, "YYYY-MM-DD"))
		scalarCheck(f, "timestamp_plus_interval", pg.Text(), q.ToChar(q.PlusAs(pg.Timestamptz(), stamp.Expr(), span.Expr()), format.Expr()), new("2024-03-11 07:45"), q.BindValue(stamp, date), q.BindValue(span, "1 day 2 hours"), q.BindValue(format, "YYYY-MM-DD HH24:MI"))
		clock := scalarPlan(f, pg.Timestamptz(), q.Now())
		current := scalarPlan(f, pg.Timestamptz(), q.CurrentTimestamp())
		today := scalarPlan(f, pg.Date(), q.CurrentDate())
		uuid := scalarPlan(f, pg.UUID[string](), q.GenRandomUUID())
		mappedUUID := scalarPlan(f, pg.UUID[scalarUUID](), q.GenRandomUUIDAs(pg.UUID[scalarUUID]()))
		require.NoError(f.t, f.run(f.t.Context(), func(ctx context.Context, s session) error {
			a, err := clock.Bind(s).Find(ctx)
			require.NoError(f.t, err)
			b, err := current.Bind(s).Find(ctx)
			require.NoError(f.t, err)
			require.Equal(f.t, a, b)
			d, err := today.Bind(s).Find(ctx)
			require.NoError(f.t, err)
			require.NotNil(f.t, d[0].Value)
			x, err := uuid.Bind(s).Find(ctx)
			require.NoError(f.t, err)
			y, err := uuid.Bind(s).Find(ctx)
			require.NoError(f.t, err)
			require.Regexp(f.t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, *x[0].Value)
			require.NotEqual(f.t, *x[0].Value, *y[0].Value)
			mapped, err := mappedUUID.Bind(s).Find(ctx)
			require.NoError(f.t, err)
			require.Len(f.t, string(*mapped[0].Value), 36)
			return nil
		}))
	})
}

type scalarUUID string
