//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/stretchr/testify/require"
)

func TestScalarCastMatrix(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		const uuid = "00000000-0000-4000-8000-000000000001"
		for _, c := range []struct{ from, to, value, want string }{
			{"text", "uuid", uuid, uuid}, {"uuid", "text", uuid, uuid},
			{"text", "date", "2024-03-10", "2024-03-10T00:00:00Z"}, {"date", "text", "2024-03-10", "2024-03-10"},
			{"text", "numeric", "123.4500", "123.4500"}, {"numeric", "text", "123.4500", "123.4500"},
			{"text", "int4", "42", "42"}, {"int4", "text", "42", "42"},
			{"text", "int8", "4294967296", "4294967296"}, {"int8", "text", "4294967296", "4294967296"},
			{"int8", "int4", "42", "42"}, {"int4", "int8", "42", "42"},
			{"json", "jsonb", `{"x":1,"x":2}`, `{"x": 2}`}, {"jsonb", "json", `{"x":1}`, `{"x": 1}`},
			{"bool", "text", "true", "true"}, {"text", "bool", "true", "true"},
			{"bpchar", "text", "x  ", "x"}, {"varchar", "text", "x  ", "x  "},
			{"json", "text", `{"x":1}`, `{"x":1}`}, {"jsonb", "text", `{"x":1}`, `{"x": 1}`},
		} {
			from, to := q.CatalogType[string]("pg_catalog", c.from, nil), q.CatalogType[string]("pg_catalog", c.to, nil)
			slot := q.Input("value", from)
			// The textual projection also tests the destination's native output
			// conversion; dedicated temporal tests scan native time.Time results.
			value := q.Cast(to, slot.Expr())
			if c.to == "date" {
				c.want = "2024-03-10"
			}
			scalarCheck(f, c.from+"_to_"+c.to, pg.Text(), q.Cast(pg.Text(), value), new(c.want), q.BindValue(slot, c.value))
		}
		for _, from := range []string{"int2", "int4", "int8", "float4", "float8", "numeric"} {
			for _, to := range []string{"int2", "int4", "int8", "float4", "float8", "numeric"} {
				slot := q.Input("number", q.CatalogType[string]("pg_catalog", from, nil))
				scalarCheck(f, from+"_to_"+to, pg.Text(), q.Cast(pg.Text(), q.Cast(q.CatalogType[string]("pg_catalog", to, nil), slot.Expr())), new("12"), q.BindValue(slot, "12"))
			}
		}
		arr := q.Input("array", pg.ArrayOf(pg.Text()))
		scalarCheck(f, "text_array_to_text", pg.Text(), q.Cast(pg.Text(), arr.Expr()), new(`{a,b}`), q.BindValue(arr, []string{"a", "b"}))
		i := q.Input("integers", pg.ArrayOf(pg.Int4()))
		scalarCheck(f, "array_element_cast", pg.Text(), q.Cast(pg.Text(), q.Cast(pg.ArrayOf(pg.Int8()), i.Expr())), new(`{1,2}`), q.BindValue(i, []int32{1, 2}))
	})
}

func TestScalarDatabaseErrors(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		a, b := q.Input("a", pg.Int4()), q.Input("b", pg.Int4())
		text, encoding := q.Input("text", pg.Text()), q.Input("encoding", pg.Text())
		plans := []struct {
			p     *q.ReadPlan[scalarValue[string], struct{}]
			args  []q.Binding
			state string
		}{
			{scalarPlan(f, pg.Text(), q.Cast(pg.Text(), q.Divide(a.Expr(), b.Expr()))), []q.Binding{q.BindValue(a, int32(1)), q.BindValue(b, int32(0))}, "22012"},
			{scalarPlan(f, pg.Text(), q.Cast(pg.Text(), q.Plus(a.Expr(), b.Expr()))), []q.Binding{q.BindValue(a, int32(2147483647)), q.BindValue(b, int32(1))}, "22003"},
			{scalarPlan(f, pg.Text(), q.Cast(pg.Text(), q.Cast(pg.UUID[string](), text.Expr()))), []q.Binding{q.BindValue(text, "invalid")}, "22P02"},
			{scalarPlan(f, pg.Text(), q.Cast(pg.Text(), q.RegexpMatch(text.Expr(), text.Expr()))), []q.Binding{q.BindValue(text, "[")}, "2201B"},
			{scalarPlan(f, pg.Text(), q.Encode(q.ConvertTo(text.Expr(), encoding.Expr()), q.Hex)), []q.Binding{q.BindValue(text, "a"), q.BindValue(encoding, "invalid_encoding")}, "22023"},
			{scalarPlan(f, pg.Text(), q.Cast(pg.Text(), q.JSONBuildObject(pg.JSONB[string](), q.JSONPair(q.TypedNull(pg.Text()), text.Expr())))), []q.Binding{q.BindValue(text, "a")}, "22023"},
		}
		for _, c := range plans {
			err := f.run(f.t.Context(), func(ctx context.Context, s session) error { _, err := c.p.Bind(s).Find(ctx, c.args...); return err })
			require.Equal(f.t, c.state, q.SQLState(err))
		}
	})
}
