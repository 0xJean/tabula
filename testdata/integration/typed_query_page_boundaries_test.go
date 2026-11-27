//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/stretchr/testify/require"
)

func TestQueryWinnerResultList(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		t := f.t
		id := q.FieldOf(f.src.Rel(), "id", pg.Int8())
		two := q.Input("two", pg.Int8())
		groupValue := q.Divide(id.Expr(), two.Expr())
		group, key := q.Export("group", groupValue), q.Export("id", id.Expr())
		d := q.Derived("winners", q.Select(group.Project(), key.Project()).From(f.src).DistinctOn(groupValue).OrderBy(q.AscExpr(groupValue), q.Desc(id)))
		out := q.DefineView(f.reg, q.ViewSpec[windowValue[int64]]{Name: "groups"})
		plan := q.DefineRead(out, "winners", q.Select(q.Project(q.Output(out, "id", pg.Int8()), key.From(d)), q.Project(q.Output(out, "value", pg.Int8()), group.From(d))).From(d).
			WithList(q.ReadListSpec{Orders: []q.RowOrder{{Name: "group", Terms: []q.RowTerm{q.AscExpr(group.From(d))}}}, Pages: q.RowPages{Keyset: true, MaxLimit: 2}, ExactCount: true}))
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			rows, count, err := plan.Bind(s).List(ctx, q.RowChoice{Order: "group", Page: q.FirstPage, Limit: 2}, q.BindValue(two, int64(2)))
			require.NoError(t, err)
			require.EqualValues(t, 4, count)
			require.Equal(t, []windowValue[int64]{{ID: 1, Value: new(int64(0))}, {ID: 3, Value: new(int64(1))}}, rows)
			cursor, err := plan.CursorFrom("group", rows[1])
			require.NoError(t, err)
			rows, count, err = plan.Bind(s).List(ctx, q.RowChoice{Order: "group", Page: q.AfterPage, Limit: 2, After: cursor}, q.BindValue(two, int64(2)))
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
		id := q.FieldOf(f.src.Rel(), "id", pg.Int8())
		key := q.Export("id", id.Expr())
		a, b := q.Export("arm", q.IsNull(q.TypedNull(pg.Int8()))), q.Export("arm", q.Not(q.IsNull(q.TypedNull(pg.Int8()))))
		d := q.Derived("combined", q.UnionAll(q.Select(key.Project(), a.Project()).From(f.src), q.Select(key.Project(), b.Project()).From(f.src)))
		out := q.DefineView(f.reg, q.ViewSpec[windowValue[bool]]{Name: "entries"})
		plan := q.DefineRead(out, "combined", q.Select(q.Project(q.Output(out, "id", pg.Int8()), key.From(d)), q.Project(q.Output(out, "value", pg.Bool()), a.From(d))).From(d).
			WithList(q.ReadListSpec{Orders: []q.RowOrder{{Name: "key", Terms: []q.RowTerm{q.AscExpr(a.From(d)), q.AscExpr(key.From(d))}}}, Pages: q.RowPages{Keyset: true, MaxLimit: 4}, ExactCount: true}))
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			choice := q.RowChoice{Order: "key", Page: q.FirstPage, Limit: 4}
			var all []windowValue[bool]
			for range 3 {
				rows, count, err := plan.Bind(s).List(ctx, choice)
				require.NoError(t, err)
				require.EqualValues(t, 12, count)
				require.Len(t, rows, 4)
				all = append(all, rows...)
				choice.After, err = plan.CursorFrom("key", rows[3])
				require.NoError(t, err)
				choice.Page = q.AfterPage
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
		first, step, last := q.Input("first", pg.Int8()), q.Input("step", pg.Int8()), q.Input("last", pg.Int8())
		value := q.Export("value", first.Expr())
		common := q.RecursiveCTE("entries", q.Select(value.Project()), q.RecursiveUnionAll, func(self q.QuerySource) q.ReadSelect {
			next := q.Plus(value.From(self), step.Expr())
			return q.Select(q.Export("value", next).Project()).From(self).Where(q.Le(next, last.Expr()))
		})
		d := common.As("d")
		out := q.DefineView(f.reg, q.ViewSpec[scalarValue[int64]]{Name: "entries"})
		query := q.Select(q.Project(q.Output(out, "value", pg.Int8()), value.From(d))).From(d).With(common)
		spec := q.ReadListSpec{Orders: []q.RowOrder{{Name: "value", Terms: []q.RowTerm{q.AscExpr(value.From(d))}}}, Pages: q.RowPages{Offset: true, MaxLimit: 2, MaxOffset: 5}, ExactCount: true}
		plan := q.DefineRead(out, "recursive", query.WithList(spec))
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			rows, count, err := plan.Bind(s).List(ctx, q.RowChoice{Order: "value", Page: q.OffsetPage, Limit: 2, Offset: 2}, q.BindValue(first, int64(1)), q.BindValue(step, int64(1)), q.BindValue(last, int64(5)))
			require.NoError(t, err)
			require.EqualValues(t, 5, count)
			require.Equal(t, []scalarValue[int64]{{Value: new(int64(3))}, {Value: new(int64(4))}}, rows)
			return nil
		}))
		spec.Pages.Keyset = true
		_, err := q.TryDefineRead(out, "unproved", query.WithList(spec))
		require.Error(t, err)
	})
}

func TestQueryFixedAndBoundedListLimits(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		windowRows(f)
		t := f.t
		id := q.FieldOf(f.src.Rel(), "id", pg.Int8())
		ids, lower, limit := q.Input("ids", q.ArrayOf(pg.Int8())), q.Input("lower", pg.Int8()), q.Input("limit", pg.Int8())
		key := q.Export("id", id.Expr())
		d := q.Derived("limited", q.Select(key.Project()).From(f.src).Where(q.Any(id.Expr(), ids.Expr())).OrderBy(q.Asc(id)).Take(5))
		value := key.From(d)
		out := q.DefineView(f.reg, q.ViewSpec[scalarValue[int64]]{Name: "entries"})
		plan := q.DefineRead(out, "bounded", q.Select(q.Project(q.Output(out, "value", pg.Int8()), value)).From(d).
			Where(q.Ge(value, lower.Expr()), q.Any(value, ids.Expr())).OrderBy(q.AscExpr(value)).Limit(limit, 5).
			WithList(q.ReadListSpec{Orders: []q.RowOrder{{Name: "value", Terms: []q.RowTerm{q.AscExpr(value)}}}, Pages: q.RowPages{Keyset: true, MaxLimit: 2}, ExactCount: true}))
		bindings := []q.Binding{q.BindValue(ids, []int64{1, 2, 3, 4, 5, 6}), q.BindValue(lower, int64(2)), q.BindValue(limit, int64(3))}
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			rows, count, err := plan.Bind(s).List(ctx, q.RowChoice{Order: "value", Page: q.FirstPage, Limit: 2}, bindings...)
			require.NoError(t, err)
			require.EqualValues(t, 3, count)
			require.Equal(t, []scalarValue[int64]{{Value: new(int64(2))}, {Value: new(int64(3))}}, rows)
			cursor, err := plan.CursorFrom("value", rows[1])
			require.NoError(t, err)
			rows, count, err = plan.Bind(s).List(ctx, q.RowChoice{Order: "value", Page: q.AfterPage, Limit: 2, After: cursor}, bindings...)
			require.NoError(t, err)
			require.EqualValues(t, 3, count)
			require.Equal(t, []scalarValue[int64]{{Value: new(int64(4))}}, rows)
			_, _, err = plan.Bind(s).List(ctx, q.RowChoice{Order: "value", Page: q.FirstPage, Limit: 2}, bindings[0], bindings[1], q.BindValue(limit, int64(6)))
			require.ErrorContains(t, err, "limit")
			return nil
		}))
	})
}
