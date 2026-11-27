//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/stretchr/testify/require"
)

func TestQueryWindowResultList(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		t := f.t
		id := tplan.FieldOf(f.src.Rel(), "id", pg.Int8())
		ids := tplan.Input("ids", q.ArrayOf(pg.Int8()))
		lower := tplan.Input("lower", pg.Int8())
		key := tplan.Export("id", id.Expr())
		position := tplan.Export("position", tplan.Over(tplan.RowNumber(), tplan.WindowSpec{Order: []tplan.RowTerm{tplan.Asc(id)}}))
		d := tplan.Derived("ranked", tplan.Select(key.Project(), position.Project()).From(f.src).Where(tplan.Any(id.Expr(), ids.Expr())))
		out := q.DefineView(f.reg, q.ViewSpec[windowValue[int64]]{Name: "positions"})
		query := tplan.Select(tplan.Project(tplan.Output(out, "id", pg.Int8()), key.From(d)), tplan.Project(tplan.Output(out, "value", pg.Int8()), position.From(d))).From(d).
			Where(tplan.Ge(position.From(d), lower.Expr()), tplan.Any(key.From(d), ids.Expr())).
			WithList(tplan.ReadListSpec{Orders: []tplan.RowOrder{{Name: "position", Terms: []tplan.RowTerm{tplan.AscExpr(position.From(d)), tplan.AscExpr(key.From(d))}}}, Pages: tplan.RowPages{Keyset: true, Offset: true, MaxLimit: 3, MaxOffset: 6}, ExactCount: true})
		plan := tplan.DefineRead(out, "ranked", query)
		values := []int64{1, 2, 3, 4, 5, 6}
		bindings := []tplan.Binding{tplan.BindValue(ids, values), tplan.BindValue(lower, int64(2))}
		values[0] = 99 // Binding already captured the canonical input.
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			bound := plan.Bind(s)
			rows, count, err := bound.List(ctx, tplan.RowChoice{Order: "position", Page: tplan.FirstPage, Limit: 2}, bindings...)
			require.NoError(t, err)
			require.EqualValues(t, 5, count)
			require.Equal(t, []windowValue[int64]{{ID: 2, Value: new(int64(2))}, {ID: 3, Value: new(int64(3))}}, rows)
			cursor, err := plan.CursorFrom("position", rows[1])
			require.NoError(t, err)
			rows, count, err = bound.List(ctx, tplan.RowChoice{Order: "position", Page: tplan.AfterPage, Limit: 2, After: cursor}, bindings...)
			require.NoError(t, err)
			require.EqualValues(t, 5, count)
			require.Equal(t, []windowValue[int64]{{ID: 4, Value: new(int64(4))}, {ID: 5, Value: new(int64(5))}}, rows)
			rows, count, err = bound.List(ctx, tplan.RowChoice{Order: "position", Page: tplan.FirstPage, Limit: 2}, tplan.BindValue(ids, []int64{}), tplan.BindValue(lower, int64(1)))
			require.NoError(t, err)
			require.Empty(t, rows)
			require.Zero(t, count)
			return nil
		}))
	})
}

func TestQuerySetAndGroupedLists(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		t := f.t
		id := tplan.FieldOf(f.src.Rel(), "id", pg.Int8())
		key := tplan.Export("value", id.Expr())
		arm := tplan.Select(key.Project()).From(f.src)
		for name, combined := range map[string]tplan.ReadSelect{
			"union": tplan.Union(arm, arm), "intersect": tplan.Intersect(arm, arm), "except": tplan.Except(arm, arm),
			"intersect_all": tplan.IntersectAll(arm, arm), "except_all": tplan.ExceptAll(arm, arm),
		} {
			d := tplan.Derived("combined", combined)
			out := q.DefineView(f.reg, q.ViewSpec[scalarValue[int64]]{Name: name + "_rows"})
			plan := tplan.DefineRead(out, name, tplan.Select(tplan.Project(tplan.Output(out, "value", pg.Int8()), key.From(d))).From(d).
				WithList(tplan.ReadListSpec{Orders: []tplan.RowOrder{{Name: "value", Terms: []tplan.RowTerm{tplan.AscExpr(key.From(d))}}}, Pages: tplan.RowPages{Keyset: true, MaxLimit: 2}, ExactCount: true}))
			require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
				rows, count, err := plan.Bind(s).List(ctx, tplan.RowChoice{Order: "value", Page: tplan.FirstPage, Limit: 2})
				require.NoError(t, err)
				if name == "except" || name == "except_all" {
					require.Zero(t, count)
					require.Empty(t, rows)
				} else {
					require.EqualValues(t, 6, count)
					require.Equal(t, []scalarValue[int64]{{Value: new(int64(1))}, {Value: new(int64(2))}}, rows)
				}
				return nil
			}))
		}
		// Grouping proves a result key even though the input relation has no
		// uniqueness declaration (an array expansion can contain duplicates).
		values := tplan.Input("values", q.ArrayOf(pg.Int8()))
		value := tplan.UnnestColumn("value", values.Expr(), pg.Int8())
		u := tplan.Unnest("u", value)
		group := tplan.Export("value", value.From(u))
		d := tplan.Derived("grouped", tplan.Select(group.Project()).From(u).GroupBy(value.From(u)))
		out := q.DefineView(f.reg, q.ViewSpec[scalarValue[int64]]{Name: "grouped_rows"})
		plan := tplan.DefineRead(out, "groups", tplan.Select(tplan.Project(tplan.Output(out, "value", pg.Int8()), group.From(d))).From(d).
			WithList(tplan.ReadListSpec{Orders: []tplan.RowOrder{{Name: "value", Terms: []tplan.RowTerm{tplan.AscExpr(group.From(d))}}}, Pages: tplan.RowPages{Keyset: true, Offset: true, MaxOffset: 4, MaxLimit: 2}, ExactCount: true}))
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			rows, count, err := plan.Bind(s).List(ctx, tplan.RowChoice{Order: "value", Page: tplan.OffsetPage, Limit: 2}, tplan.BindValue(values, []int64{1, 1, 2, 3}))
			require.NoError(t, err)
			require.EqualValues(t, 3, count)
			require.Len(t, rows, 2)
			return nil
		}))
	})
}

func TestQueryOuterNullList(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		t := f.t
		id := tplan.FieldOf(f.src.Rel(), "id", pg.Int8())
		three := tplan.Input("three", pg.Int8())
		key := tplan.Export("id", id.Expr())
		d := tplan.Derived("d", tplan.Select(key.Project()).From(f.src).Where(tplan.Le(id.Expr(), three.Expr())))
		value := key.From(d)
		out := q.DefineView(f.reg, q.ViewSpec[windowValue[int64]]{Name: "outer_rows"})
		plan := tplan.DefineRead(out, "outer", tplan.Select(tplan.Project(tplan.Output(out, "id", pg.Int8()), id.Expr()), tplan.Project(tplan.Output(out, "value", pg.Int8()), value)).From(f.src).
			LeftJoin(d, tplan.Eq(id.Expr(), value)).WithList(tplan.ReadListSpec{Orders: []tplan.RowOrder{{Name: "key", Terms: []tplan.RowTerm{tplan.AscExpr(value).NullsFirst(), tplan.Desc(id)}}}, Pages: tplan.RowPages{Keyset: true, MaxLimit: 2}, ExactCount: true}))
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			var all []windowValue[int64]
			choice := tplan.RowChoice{Order: "key", Page: tplan.FirstPage, Limit: 2}
			for range 3 {
				rows, count, err := plan.Bind(s).List(ctx, choice, tplan.BindValue(three, int64(3)))
				require.NoError(t, err)
				require.EqualValues(t, 6, count)
				require.Len(t, rows, 2)
				all = append(all, rows...)
				choice.After, err = plan.CursorFrom("key", rows[1])
				require.NoError(t, err)
				choice.Page = tplan.AfterPage
			}
			require.Equal(t, []windowValue[int64]{{ID: 6}, {ID: 5}, {ID: 4}, {ID: 1, Value: new(int64(1))}, {ID: 2, Value: new(int64(2))}, {ID: 3, Value: new(int64(3))}}, all)
			return nil
		}))
	})
}
