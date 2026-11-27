package tabula

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlanSourceRejectsLegacy(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	table := DefinePlanSource(reg, TableSpec[modeItem, int64]{Name: "public.items", AllowDelete: true})
	before := table.Definition()
	require.PanicsWithValue(t, ErrOperationDenied, func() { table.Statement("raw", "SELECT 1") })
	require.PanicsWithValue(t, ErrOperationDenied, func() { table.Requires("public.items", "SELECT", "id") })
	require.PanicsWithValue(t, ErrOperationDenied, func() { table.RequiresFunction("public.lookup()") })
	_, err := table.TryDefineUpsert("put", UpsertSpec{})
	require.ErrorIs(t, err, ErrOperationDenied)
	require.Equal(t, before, table.Definition())

	s := &fakeSession{}
	r, ctx := table.Bind(s), t.Context()
	filter := func(*Filter) { t.Fatal("denied call evaluated filter") }
	deny := func(err error) { t.Helper(); require.ErrorIs(t, err, ErrOperationDenied) }
	_, err = r.Get(ctx, 1)
	deny(err)
	_, err = r.LockMany(ctx, nil)
	deny(err)
	_, _, err = r.List(ctx, filter, Listing{})
	deny(err)
	_, err = collect(r.Iter(ctx, filter, Listing{}))
	deny(err)
	_, err = r.ListKeyset(ctx, filter, KeysetListing{})
	deny(err)
	_, _, err = Max[int64](ctx, r, "rank", filter)
	deny(err)
	deny(r.CheckCopy())
	deny(r.CheckWrite(nil))
	deny(r.Insert(ctx, nil))
	deny(r.InsertMany(ctx, nil))
	deny(r.Save(ctx, modeItem{}))
	deny(r.Set(ctx, modeItem{}, "rank"))
	deny(r.Delete(ctx, 1))
	_, err = r.UpsertMany(ctx, nil, nil)
	deny(err)
	deny(r.Exec(ctx, 0, "SELECT 1"))
	deny(r.Scan(ctx, "SELECT 1", nil))
	_, err = collect(r.SelectIter(ctx, "SELECT 1"))
	deny(err)
	require.Empty(t, s.lastQuery)
}

func TestPlanSourceValidation(t *testing.T) {
	reg := NewRegistry(RegistryConfig[struct{}]{})
	for _, spec := range []TableSpec[strictItem, int64]{
		{Name: "items", RelationScope: Global()}, {Name: "public.items"},
		{Name: "public.items", RelationScope: ByScopeColumn()},
		{Name: "public.items", RelationScope: Global(), Order: "id"},
		{Name: "public.items", RelationScope: Global(), GroupBy: []string{"id"}},
		{Name: "public.items", RelationScope: Global(), Aggregates: ScalarAggregates{Max: []string{"id"}}},
		{Name: "public.items", RelationScope: Global(), AllowDelete: true, ReadOnly: true},
		{Name: "public.items", RelationScope: Global(), WriteMode: WriteUpdateOnly, UpdateColumns: []string{"id"}},
	} {
		table, err := TryDefinePlanSource(reg, spec)
		require.Error(t, err)
		require.Nil(t, table)
		require.Panics(t, func() { DefinePlanSource(reg, spec) })
	}
	require.Empty(t, reg.Defined())
}
