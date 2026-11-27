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

func TestQueryWinnerResultList(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		t := f.t
		id := tplan.FieldOf(f.src.Rel(), "id", pg.Int8())
		two := tplan.Input("two", pg.Int8())
		groupValue := tplan.Divide(id.Expr(), two.Expr())
		group, key := tplan.Export("group", groupValue), tplan.Export("id", id.Expr())
		d := tplan.Derived("winners", tplan.Select(group.Project(), key.Project()).From(f.src).DistinctOn(groupValue).OrderBy(tplan.AscExpr(groupValue), tplan.Desc(id)))
		out := q.DefineView(f.reg, q.ViewSpec[windowValue[int64]]{Name: "groups"})
		plan := tplan.DefineRead(out, "winners", tplan.Select(tplan.Project(tplan.Output(out, "id", pg.Int8()), key.From(d)), tplan.Project(tplan.Output(out, "value", pg.Int8()), group.From(d))).From(d).
			WithList(tplan.ReadListSpec{Orders: []tplan.RowOrder{{Name: "group", Terms: []tplan.RowTerm{tplan.AscExpr(group.From(d))}}}, Pages: tplan.RowPages{Keyset: true, MaxLimit: 2}, ExactCount: true}))
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			rows, count, err := plan.Bind(s).List(ctx, tplan.RowChoice{Order: "group", Page: tplan.FirstPage, Limit: 2}, tplan.BindValue(two, int64(2)))
			require.NoError(t, err)
			require.EqualValues(t, 4, count)
			require.Equal(t, []windowValue[int64]{{ID: 1, Value: new(int64(0))}, {ID: 3, Value: new(int64(1))}}, rows)
			cursor, err := plan.CursorFrom("group", rows[1])
			require.NoError(t, err)
			rows, count, err = plan.Bind(s).List(ctx, tplan.RowChoice{Order: "group", Page: tplan.AfterPage, Limit: 2, After: cursor}, tplan.BindValue(two, int64(2)))
			require.NoError(t, err)
			require.EqualValues(t, 4, count)
			require.Equal(t, []windowValue[int64]{{ID: 5, Value: new(int64(2))}, {ID: 6, Value: new(int64(3))}}, rows)
			return nil
		}))
	})
}

func TestQueryUnionAllResultList(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		t := f.t
		id := tplan.FieldOf(f.src.Rel(), "id", pg.Int8())
		key := tplan.Export("id", id.Expr())
		a, b := tplan.Export("arm", tplan.IsNull(tplan.TypedNull(pg.Int8()))), tplan.Export("arm", tplan.Not(tplan.IsNull(tplan.TypedNull(pg.Int8()))))
		d := tplan.Derived("combined", tplan.UnionAll(tplan.Select(key.Project(), a.Project()).From(f.src), tplan.Select(key.Project(), b.Project()).From(f.src)))
		out := q.DefineView(f.reg, q.ViewSpec[windowValue[bool]]{Name: "entries"})
		plan := tplan.DefineRead(out, "combined", tplan.Select(tplan.Project(tplan.Output(out, "id", pg.Int8()), key.From(d)), tplan.Project(tplan.Output(out, "value", pg.Bool()), a.From(d))).From(d).
			WithList(tplan.ReadListSpec{Orders: []tplan.RowOrder{{Name: "key", Terms: []tplan.RowTerm{tplan.AscExpr(a.From(d)), tplan.AscExpr(key.From(d))}}}, Pages: tplan.RowPages{Keyset: true, MaxLimit: 4}, ExactCount: true}))
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			choice := tplan.RowChoice{Order: "key", Page: tplan.FirstPage, Limit: 4}
			var all []windowValue[bool]
			for range 3 {
				rows, count, err := plan.Bind(s).List(ctx, choice)
				require.NoError(t, err)
				require.EqualValues(t, 12, count)
				require.Len(t, rows, 4)
				all = append(all, rows...)
				choice.After, err = plan.CursorFrom("key", rows[3])
				require.NoError(t, err)
				choice.Page = tplan.AfterPage
			}
			for i, row := range all {
				require.EqualValues(t, i%6+1, row.ID)
				require.Equal(t, new(i >= 6), row.Value)
			}
			return nil
		}))
	})
}

func TestQueryRecursiveOffsetList(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		t := f.t
		first, step, last := tplan.Input("first", pg.Int8()), tplan.Input("step", pg.Int8()), tplan.Input("last", pg.Int8())
		value := tplan.Export("value", first.Expr())
		common := tplan.RecursiveCTE("entries", tplan.Select(value.Project()), tplan.RecursiveUnionAll, func(self tplan.QuerySource) tplan.ReadSelect {
			next := tplan.Plus(value.From(self), step.Expr())
			return tplan.Select(tplan.Export("value", next).Project()).From(self).Where(tplan.Le(next, last.Expr()))
		})
		d := common.As("d")
		out := q.DefineView(f.reg, q.ViewSpec[scalarValue[int64]]{Name: "entries"})
		query := tplan.Select(tplan.Project(tplan.Output(out, "value", pg.Int8()), value.From(d))).From(d).With(common)
		spec := tplan.ReadListSpec{Orders: []tplan.RowOrder{{Name: "value", Terms: []tplan.RowTerm{tplan.AscExpr(value.From(d))}}}, Pages: tplan.RowPages{Offset: true, MaxLimit: 2, MaxOffset: 5}, ExactCount: true}
		plan := tplan.DefineRead(out, "recursive", query.WithList(spec))
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			rows, count, err := plan.Bind(s).List(ctx, tplan.RowChoice{Order: "value", Page: tplan.OffsetPage, Limit: 2, Offset: 2}, tplan.BindValue(first, int64(1)), tplan.BindValue(step, int64(1)), tplan.BindValue(last, int64(5)))
			require.NoError(t, err)
			require.EqualValues(t, 5, count)
			require.Equal(t, []scalarValue[int64]{{Value: new(int64(3))}, {Value: new(int64(4))}}, rows)
			return nil
		}))
		spec.Pages.Keyset = true
		_, err := tplan.TryDefineRead(out, "unproved", query.WithList(spec))
		require.Error(t, err)
	})
}

func TestQueryFixedAndBoundedListLimits(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		t := f.t
		id := tplan.FieldOf(f.src.Rel(), "id", pg.Int8())
		ids, lower, limit := tplan.Input("ids", q.ArrayOf(pg.Int8())), tplan.Input("lower", pg.Int8()), tplan.Input("limit", pg.Int8())
		key := tplan.Export("id", id.Expr())
		d := tplan.Derived("limited", tplan.Select(key.Project()).From(f.src).Where(tplan.Any(id.Expr(), ids.Expr())).OrderBy(tplan.Asc(id)).Take(5))
		value := key.From(d)
		out := q.DefineView(f.reg, q.ViewSpec[scalarValue[int64]]{Name: "entries"})
		plan := tplan.DefineRead(out, "bounded", tplan.Select(tplan.Project(tplan.Output(out, "value", pg.Int8()), value)).From(d).
			Where(tplan.Ge(value, lower.Expr()), tplan.Any(value, ids.Expr())).OrderBy(tplan.AscExpr(value)).Limit(limit, 5).
			WithList(tplan.ReadListSpec{Orders: []tplan.RowOrder{{Name: "value", Terms: []tplan.RowTerm{tplan.AscExpr(value)}}}, Pages: tplan.RowPages{Keyset: true, MaxLimit: 2}, ExactCount: true}))
		bindings := []tplan.Binding{tplan.BindValue(ids, []int64{1, 2, 3, 4, 5, 6}), tplan.BindValue(lower, int64(2)), tplan.BindValue(limit, int64(3))}
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			rows, count, err := plan.Bind(s).List(ctx, tplan.RowChoice{Order: "value", Page: tplan.FirstPage, Limit: 2}, bindings...)
			require.NoError(t, err)
			require.EqualValues(t, 3, count)
			require.Equal(t, []scalarValue[int64]{{Value: new(int64(2))}, {Value: new(int64(3))}}, rows)
			cursor, err := plan.CursorFrom("value", rows[1])
			require.NoError(t, err)
			rows, count, err = plan.Bind(s).List(ctx, tplan.RowChoice{Order: "value", Page: tplan.AfterPage, Limit: 2, After: cursor}, bindings...)
			require.NoError(t, err)
			require.EqualValues(t, 3, count)
			require.Equal(t, []scalarValue[int64]{{Value: new(int64(4))}}, rows)
			_, _, err = plan.Bind(s).List(ctx, tplan.RowChoice{Order: "value", Page: tplan.FirstPage, Limit: 2}, bindings[0], bindings[1], tplan.BindValue(limit, int64(6)))
			require.ErrorContains(t, err, "limit")
			return nil
		}))
	})
}
