//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/plan"
	"github.com/stretchr/testify/require"
)

func TestScalarJSONAndArrays(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		doc := plan.Input("doc", pg.JSONB[json.RawMessage]())
		key, path, index := plan.Input("key", pg.Text()), plan.Input("path", pg.ArrayOf(pg.Text())), plan.Input("index", pg.Int4())
		value := plan.BindValue(doc, json.RawMessage(`{"a":[1,null,3],"b":null}`))
		scalarCheck(f, "get", pg.Text(), plan.Cast(pg.Text(), plan.JSONGet(doc.Expr(), key.Expr())), new("[1, null, 3]"), value, plan.BindValue(key, "a"))
		scalarCheck(f, "missing", pg.Text(), plan.JSONGetText(doc.Expr(), key.Expr()), nil, value, plan.BindValue(key, "missing"))
		scalarCheck(f, "json_null", pg.Text(), plan.Cast(pg.Text(), plan.JSONGet(doc.Expr(), key.Expr())), new("null"), value, plan.BindValue(key, "b"))
		scalarCheck(f, "json_null_text", pg.Text(), plan.JSONGetText(doc.Expr(), key.Expr()), nil, value, plan.BindValue(key, "b"))
		scalarCheck(f, "path", pg.Text(), plan.JSONGetTextPath(doc.Expr(), path.Expr()), new("3"), value, plan.BindValue(path, []string{"a", "-1"}))
		scalarCheck(f, "path_json", pg.Text(), plan.Cast(pg.Text(), plan.JSONGetPath(doc.Expr(), path.Expr())), new("null"), value, plan.BindValue(path, []string{"a", "1"}))
		arrayDoc := plan.BindValue(doc, json.RawMessage(`[1,2,3]`))
		scalarCheck(f, "at", pg.Text(), plan.Cast(pg.Text(), plan.JSONGetAt(doc.Expr(), index.Expr())), new("3"), arrayDoc, plan.BindValue(index, int32(-1)))
		scalarCheck(f, "at_text", pg.Text(), plan.JSONGetTextAt(doc.Expr(), index.Expr()), new("2"), arrayDoc, plan.BindValue(index, int32(1)))
		scalarCheck(f, "has", pg.Bool(), plan.JSONHasKey(doc.Expr(), key.Expr()), new(true), value, plan.BindValue(key, "b"))
		scalarCheck(f, "contains", pg.Bool(), plan.JSONContains(doc.Expr(), doc.Expr()), new(true), value)
		scalarCheck(f, "contained", pg.Bool(), plan.JSONContainedBy(doc.Expr(), doc.Expr()), new(true), value)
		scalarCheck(f, "type", pg.Text(), plan.JSONTypeOf(doc.Expr()), new("object"), value)
		scalarCheck(f, "cat", pg.Text(), plan.Cast(pg.Text(), plan.JSONBCat(doc.Expr(), doc.Expr())), new("[1, 2, 3, 1, 2, 3]"), arrayDoc)
		scalarCheck(f, "delete_key", pg.Text(), plan.Cast(pg.Text(), plan.JSONBDeleteKey(doc.Expr(), key.Expr())), new(`{"b": null}`), value, plan.BindValue(key, "a"))
		scalarCheck(f, "delete_keys", pg.Text(), plan.Cast(pg.Text(), plan.JSONBDeleteKeys(doc.Expr(), path.Expr())), new(`{}`), value, plan.BindValue(path, []string{"a", "b"}))
		scalarCheck(f, "delete_at", pg.Text(), plan.Cast(pg.Text(), plan.JSONBDeleteAt(doc.Expr(), index.Expr())), new(`[1, 2]`), arrayDoc, plan.BindValue(index, int32(-1)))
		scalarCheck(f, "delete_path", pg.Text(), plan.Cast(pg.Text(), plan.JSONBDeletePath(doc.Expr(), path.Expr())), new(`{"a": [1, 3], "b": null}`), value, plan.BindValue(path, []string{"a", "1"}))
		first, second := plan.Input("first", pg.Int4()), plan.Input("second", pg.Int4())
		pairs := []plan.JSONMember{plan.JSONPair(key.Expr(), first.Expr()), plan.JSONPair(key.Expr(), second.Expr())}
		args := []plan.Binding{plan.BindValue(key, "x"), plan.BindValue(first, int32(1)), plan.BindValue(second, int32(2))}
		scalarCheck(f, "build_json", pg.Text(), plan.Cast(pg.Text(), plan.JSONBuildObject(pg.JSON[json.RawMessage](), pairs...)), new(`{"x" : 1, "x" : 2}`), args...)
		scalarCheck(f, "build_jsonb", pg.Text(), plan.Cast(pg.Text(), plan.JSONBuildObject(pg.JSONB[json.RawMessage](), pairs...)), new(`{"x": 2}`), args...)
		scalarCheck(f, "build_empty", pg.Text(), plan.Cast(pg.Text(), plan.JSONBuildObject(pg.JSONB[json.RawMessage]())), new(`{}`))
		scalarCheck(f, "build_null_value", pg.Text(), plan.Cast(pg.Text(), plan.JSONBuildObject(pg.JSONB[json.RawMessage](), plan.JSONPair(key.Expr(), plan.TypedNull(pg.Int4())))), new(`{"x": null}`), plan.BindValue(key, "x"))
		arr := plan.Input("array", pg.ArrayOf(pg.Int4()))
		arrayBinding := plan.BindValue(arr, []int32{1, 2, 1})
		scalarCheck(f, "array_expr", pg.Text(), plan.Cast(pg.Text(), plan.ArrayExpr(first.Expr(), plan.TypedNull(pg.Int4()), first.Expr())), new(`{1,NULL,1}`), plan.BindValue(first, int32(1)))
		scalarCheck(f, "array_cat", pg.Text(), plan.Cast(pg.Text(), plan.ArrayCat(arr.Expr(), plan.TypedNull(pg.ArrayOf(pg.Int4())))), new(`{1,2,1}`), arrayBinding)
		scalarCheck(f, "array_contains", pg.Bool(), plan.ArrayContains(arr.Expr(), plan.ArrayExpr(first.Expr())), new(true), arrayBinding, plan.BindValue(first, int32(1)))
		scalarCheck(f, "array_contained", pg.Bool(), plan.ArrayContainedBy(plan.ArrayExpr(first.Expr()), arr.Expr()), new(true), arrayBinding, plan.BindValue(first, int32(1)))
		scalarCheck(f, "array_overlap", pg.Bool(), plan.ArrayOverlaps(arr.Expr(), plan.ArrayExpr(first.Expr())), new(false), arrayBinding, plan.BindValue(first, int32(3)))
		scalarCheck(f, "cardinality", pg.Int4(), plan.Cardinality(arr.Expr()), new(int32(3)), arrayBinding)
		scalarCheck(f, "empty_cardinality", pg.Int4(), plan.Cardinality(arr.Expr()), new(int32(0)), plan.BindValue(arr, []int32{}))
		scalarCheck(f, "null_cardinality", pg.Int4(), plan.Cardinality(arr.Expr()), nil, plan.BindValue(arr, []int32(nil)))
		scalarCheck(f, "remove", pg.Text(), plan.Cast(pg.Text(), plan.ArrayRemove(arr.Expr(), first.Expr())), new(`{2}`), arrayBinding, plan.BindValue(first, int32(1)))
		scalarCheck(f, "remove_null", pg.Text(), plan.Cast(pg.Text(), plan.ArrayRemove(plan.ArrayExpr(first.Expr(), plan.TypedNull(pg.Int4())), plan.TypedNull(pg.Int4()))), new(`{1}`), plan.BindValue(first, int32(1)))
		for _, c := range []struct {
			name string
			expr plan.Expr[[]int32]
			want *bool
			args []plan.Binding
		}{
			{"all_empty", arr.Expr(), new(true), []plan.Binding{plan.BindValue(arr, []int32{})}},
			{"all_null", arr.Expr(), nil, []plan.Binding{plan.BindValue(arr, []int32(nil))}},
			{"all_match", plan.ArrayExpr(first.Expr(), plan.TypedNull(pg.Int4())), new(false), nil},
			{"all_unknown", plan.ArrayExpr(plan.TypedNull(pg.Int4())), nil, nil},
		} {
			scalarCheck(f, c.name, pg.Bool(), plan.NotEqualAll(first.Expr(), c.expr), c.want, append(c.args, plan.BindValue(first, int32(1)))...)
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
		day := plan.Input("day", pg.Date())
		stamp := plan.Input("stamp", pg.Timestamptz())
		format, zone := plan.Input("format", pg.Text()), plan.Input("zone", pg.Text())
		date := time.Date(2024, 3, 10, 0, 0, 0, 0, time.UTC)
		scalarCheck(f, "date_format", pg.Text(), plan.ToChar(day.Expr(), format.Expr()), new("2024-03-10 +0545 +05:45"), plan.BindValue(day, date), plan.BindValue(format, "YYYY-MM-DD TZ OF"))
		scalarCheck(f, "timestamp_format", pg.Text(), plan.ToChar(plan.Cast(pg.Timestamp(), day.Expr()), format.Expr()), new("2024-03-10  +00"), plan.BindValue(day, date), plan.BindValue(format, "YYYY-MM-DD TZ OF"))
		scalarCheck(f, "truncate", pg.Text(), plan.ToChar(plan.DateTrunc(plan.Day, stamp.Expr()), format.Expr()), new("2024-03-10 00:00"), plan.BindValue(stamp, date), plan.BindValue(format, "YYYY-MM-DD HH24:MI"))
		scalarCheck(f, "truncate_zone", pg.Text(), plan.ToChar(plan.DateTruncAt(plan.Day, stamp.Expr(), zone.Expr()), format.Expr()), new("2024-03-09 10:45"), plan.BindValue(stamp, date), plan.BindValue(zone, "America/New_York"), plan.BindValue(format, "YYYY-MM-DD HH24:MI"))
		for _, c := range []struct {
			name string
			day  int
			want string
		}{
			{"zone_before_dst", 10, "2024-03-10 10:45"},
			{"zone_after_dst", 11, "2024-03-11 09:45"},
		} {
			scalarCheck(f, c.name, pg.Text(), plan.ToChar(plan.DateTruncAt(plan.Day, stamp.Expr(), zone.Expr()), format.Expr()), new(c.want),
				plan.BindValue(stamp, time.Date(2024, 3, c.day, 12, 0, 0, 0, time.UTC)), plan.BindValue(zone, "America/New_York"), plan.BindValue(format, "YYYY-MM-DD HH24:MI"))
		}
		scalarCheck(f, "timezone", pg.Text(), plan.ToChar(plan.Timezone(zone.Expr(), stamp.Expr()), format.Expr()), new("2024-03-09 19:00"), plan.BindValue(stamp, date), plan.BindValue(zone, "America/New_York"), plan.BindValue(format, "YYYY-MM-DD HH24:MI"))
		scalarCheck(f, "extract", pg.Numeric[string](), plan.Extract(pg.Numeric[string](), plan.Year, stamp.Expr()), new("2024"), plan.BindValue(stamp, date))
		span := plan.Input("span", pg.Interval())
		scalarCheck(f, "truncate_interval", pg.Text(), plan.Cast(pg.Text(), plan.DateTrunc(plan.Hour, span.Expr())), new("1 day 02:00:00"), plan.BindValue(span, "1 day 02:35:42"))
		scalarCheck(f, "extract_interval", pg.Numeric[string](), plan.Extract(pg.Numeric[string](), plan.Hour, span.Expr()), new("2"), plan.BindValue(span, "1 day 02:35:42"))
		zoned := plan.Input("zoned", pg.Timetz())
		scalarCheck(f, "timezone_timetz", pg.Text(), plan.Cast(pg.Text(), plan.Timezone(zone.Expr(), zoned.Expr())), new("10:00:00+00"), plan.BindValue(zone, "UTC"), plan.BindValue(zoned, "12:00:00+02"))
		scalarCheck(f, "civil_timezone", pg.Text(), plan.ToChar(plan.Timezone(zone.Expr(), plan.Cast(pg.Timestamp(), day.Expr())), format.Expr()), new("2024-03-10 05:45"), plan.BindValue(day, date), plan.BindValue(zone, "UTC"), plan.BindValue(format, "YYYY-MM-DD HH24:MI"))
		scalarCheck(f, "date_plus_integer", pg.Text(), plan.ToChar(plan.PlusAs(pg.Date(), day.Expr(), plan.Cast(pg.Int4(), plan.Extract(pg.Numeric[string](), plan.Month, day.Expr()))), format.Expr()), new("2024-03-13"), plan.BindValue(day, date), plan.BindValue(format, "YYYY-MM-DD"))
		scalarCheck(f, "timestamp_plus_interval", pg.Text(), plan.ToChar(plan.PlusAs(pg.Timestamptz(), stamp.Expr(), span.Expr()), format.Expr()), new("2024-03-11 07:45"), plan.BindValue(stamp, date), plan.BindValue(span, "1 day 2 hours"), plan.BindValue(format, "YYYY-MM-DD HH24:MI"))
		clock := scalarPlan(f, pg.Timestamptz(), plan.Now())
		current := scalarPlan(f, pg.Timestamptz(), plan.CurrentTimestamp())
		today := scalarPlan(f, pg.Date(), plan.CurrentDate())
		uuid := scalarPlan(f, pg.UUID[string](), plan.GenRandomUUID())
		mappedUUID := scalarPlan(f, pg.UUID[scalarUUID](), plan.GenRandomUUIDAs(pg.UUID[scalarUUID]()))
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
