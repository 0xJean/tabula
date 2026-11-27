//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/stretchr/testify/require"
)

func TestQueryWindowResultList(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		t := f.t
		id := q.FieldOf(f.src.Rel(), "id", pg.Int8())
		ids := q.Input("ids", q.ArrayOf(pg.Int8()))
		lower := q.Input("lower", pg.Int8())
		key := q.Export("id", id.Expr())
		position := q.Export("position", q.Over(q.RowNumber(), q.WindowSpec{Order: []q.RowTerm{q.Asc(id)}}))
		d := q.Derived("ranked", q.Select(key.Project(), position.Project()).From(f.src).Where(q.Any(id.Expr(), ids.Expr())))
		out := q.DefineView(f.reg, q.ViewSpec[windowValue[int64]]{Name: "positions"})
		query := q.Select(q.Project(q.Output(out, "id", pg.Int8()), key.From(d)), q.Project(q.Output(out, "value", pg.Int8()), position.From(d))).From(d).
			Where(q.Ge(position.From(d), lower.Expr()), q.Any(key.From(d), ids.Expr())).
			WithList(q.ReadListSpec{Orders: []q.RowOrder{{Name: "position", Terms: []q.RowTerm{q.AscExpr(position.From(d)), q.AscExpr(key.From(d))}}}, Pages: q.RowPages{Keyset: true, Offset: true, MaxLimit: 3, MaxOffset: 6}, ExactCount: true})
		plan := q.DefineRead(out, "ranked", query)
		values := []int64{1, 2, 3, 4, 5, 6}
		bindings := []q.Binding{q.BindValue(ids, values), q.BindValue(lower, int64(2))}
		values[0] = 99 // Binding already captured the canonical input.
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			bound := plan.Bind(s)
			rows, count, err := bound.List(ctx, q.RowChoice{Order: "position", Page: q.FirstPage, Limit: 2}, bindings...)
			require.NoError(t, err)
			require.EqualValues(t, 5, count)
			require.Equal(t, []windowValue[int64]{{ID: 2, Value: new(int64(2))}, {ID: 3, Value: new(int64(3))}}, rows)
			cursor, err := plan.CursorFrom("position", rows[1])
			require.NoError(t, err)
			rows, count, err = bound.List(ctx, q.RowChoice{Order: "position", Page: q.AfterPage, Limit: 2, After: cursor}, bindings...)
			require.NoError(t, err)
			require.EqualValues(t, 5, count)
			require.Equal(t, []windowValue[int64]{{ID: 4, Value: new(int64(4))}, {ID: 5, Value: new(int64(5))}}, rows)
			rows, count, err = bound.List(ctx, q.RowChoice{Order: "position", Page: q.FirstPage, Limit: 2}, q.BindValue(ids, []int64{}), q.BindValue(lower, int64(1)))
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
		id := q.FieldOf(f.src.Rel(), "id", pg.Int8())
		key := q.Export("value", id.Expr())
		arm := q.Select(key.Project()).From(f.src)
		for name, combined := range map[string]q.ReadSelect{
			"union": q.Union(arm, arm), "intersect": q.Intersect(arm, arm), "except": q.Except(arm, arm),
			"intersect_all": q.IntersectAll(arm, arm), "except_all": q.ExceptAll(arm, arm),
		} {
			d := q.Derived("combined", combined)
			out := q.DefineView(f.reg, q.ViewSpec[scalarValue[int64]]{Name: name + "_rows"})
			plan := q.DefineRead(out, name, q.Select(q.Project(q.Output(out, "value", pg.Int8()), key.From(d))).From(d).
				WithList(q.ReadListSpec{Orders: []q.RowOrder{{Name: "value", Terms: []q.RowTerm{q.AscExpr(key.From(d))}}}, Pages: q.RowPages{Keyset: true, MaxLimit: 2}, ExactCount: true}))
			require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
				rows, count, err := plan.Bind(s).List(ctx, q.RowChoice{Order: "value", Page: q.FirstPage, Limit: 2})
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
		values := q.Input("values", q.ArrayOf(pg.Int8()))
		value := q.UnnestColumn("value", values.Expr(), pg.Int8())
		u := q.Unnest("u", value)
		group := q.Export("value", value.From(u))
		d := q.Derived("grouped", q.Select(group.Project()).From(u).GroupBy(value.From(u)))
		out := q.DefineView(f.reg, q.ViewSpec[scalarValue[int64]]{Name: "grouped_rows"})
		plan := q.DefineRead(out, "groups", q.Select(q.Project(q.Output(out, "value", pg.Int8()), group.From(d))).From(d).
			WithList(q.ReadListSpec{Orders: []q.RowOrder{{Name: "value", Terms: []q.RowTerm{q.AscExpr(group.From(d))}}}, Pages: q.RowPages{Keyset: true, Offset: true, MaxOffset: 4, MaxLimit: 2}, ExactCount: true}))
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			rows, count, err := plan.Bind(s).List(ctx, q.RowChoice{Order: "value", Page: q.OffsetPage, Limit: 2}, q.BindValue(values, []int64{1, 1, 2, 3}))
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
		id := q.FieldOf(f.src.Rel(), "id", pg.Int8())
		three := q.Input("three", pg.Int8())
		key := q.Export("id", id.Expr())
		d := q.Derived("d", q.Select(key.Project()).From(f.src).Where(q.Le(id.Expr(), three.Expr())))
		value := key.From(d)
		out := q.DefineView(f.reg, q.ViewSpec[windowValue[int64]]{Name: "outer_rows"})
		plan := q.DefineRead(out, "outer", q.Select(q.Project(q.Output(out, "id", pg.Int8()), id.Expr()), q.Project(q.Output(out, "value", pg.Int8()), value)).From(f.src).
			LeftJoin(d, q.Eq(id.Expr(), value)).WithList(q.ReadListSpec{Orders: []q.RowOrder{{Name: "key", Terms: []q.RowTerm{q.AscExpr(value).NullsFirst(), q.Desc(id)}}}, Pages: q.RowPages{Keyset: true, MaxLimit: 2}, ExactCount: true}))
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			var all []windowValue[int64]
			choice := q.RowChoice{Order: "key", Page: q.FirstPage, Limit: 2}
			for range 3 {
				rows, count, err := plan.Bind(s).List(ctx, choice, q.BindValue(three, int64(3)))
				require.NoError(t, err)
				require.EqualValues(t, 6, count)
				require.Len(t, rows, 2)
				all = append(all, rows...)
				choice.After, err = plan.CursorFrom("key", rows[1])
				require.NoError(t, err)
				choice.Page = q.AfterPage
			}
			require.Equal(t, []windowValue[int64]{{ID: 6}, {ID: 5}, {ID: 4}, {ID: 1, Value: new(int64(1))}, {ID: 2, Value: new(int64(2))}, {ID: 3, Value: new(int64(3))}}, all)
			return nil
		}))
	})
}
