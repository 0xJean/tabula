package tabula

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlanSourceDeclaration(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	spec := TableSpec[modeItem, int64]{Name: "public.items", WriteMode: WriteUpdateOnly, UpdateColumns: []string{"rank", "revision"}}
	legacy := Define(reg, spec).Definition()
	source, err := TryDefinePlanSource(reg, spec)
	require.NoError(t, err)
	d := source.Definition()
	require.Equal(t, legacy.Columns, d.Columns)
	require.Empty(t, d.Statements)
	require.Empty(t, d.Privileges)
	require.Empty(t, source.Insert())
	require.Equal(t, []Definition{legacy, d}, reg.Defined())

	rel := FromTable(source, "i", ByScopeColumn())
	scope := FieldOf(rel, "scope_id", CatalogType[customTenant]("pg_catalog", "text", nil))
	src := RowsFrom(source, rel, scope.Column())
	id, rank := FieldOf(rel, "id", Int8()), FieldOf(rel, "rank", Int8())
	rev := FieldOf(rel, "revision", CatalogType[int]("pg_catalog", "int8", nil))
	slot := Input("value", Int8())
	filter := RowFilter{Where: Eq(id.Expr(), slot.Expr())}
	mutation := MutationSpec{RowFilter: filter, Version: ExpectVersion(rev, Input("version", CatalogType[int]("pg_catalog", "int8", nil)))}
	_, err = TryDefineUpdateMany(src, "change", mutation, RowSet{"rank", []RowAssignment{Assign(rank, slot.Expr())}})
	require.NoError(t, err)
	key := "change.scoped.set.rank"
	d = source.Definition()
	require.Len(t, d.Statements, 1)
	require.Contains(t, d.Statements[key], `"i"."removed" = FALSE`)
	require.Contains(t, d.Statements[key], `"i"."scope_id"`)
	require.Contains(t, d.Statements[key], `"i"."revision" + 1`)
	var read []string
	for _, p := range d.StatementShapes[key].Privileges {
		if p.Privilege == "SELECT" {
			read = p.Columns
		}
	}
	require.ElementsMatch(t, []string{"id", "scope_id", "removed", "revision"}, read)
	label := FieldOf(rel, "label", Text())
	_, err = TryDefineUpdateMany(src, "denied", mutation, RowSet{"label", []RowAssignment{Assign(label, Input("label", Text()).Expr())}})
	require.ErrorIs(t, err, ErrOperationDenied)
	_, err = TryDefineDeleteMany(src, "remove", mutation)
	require.ErrorIs(t, err, ErrOperationDenied)
	require.Equal(t, d, source.Definition())
	require.Equal(t, legacy, reg.Defined()[0])
}

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
	for _, spec := range []TableSpec[typedItem, int64]{
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

func TestPlanSourceTypedWriteLimits(t *testing.T) {
	reg := NewRegistry(RegistryConfig[struct{}]{})
	for _, spec := range []TableSpec[typedItem, int64]{
		{Name: "public.items", RelationScope: Global(), ReadOnly: true},
		{Name: "public.items", RelationScope: Global(), WriteMode: WriteInsertOnly},
		{Name: "public.items", RelationScope: Global(), AllowDelete: true},
	} {
		table := DefinePlanSource(reg, spec)
		rel := FromTable(table, "i", Global())
		id := FieldOf(rel, "id", Int8())
		src := RowsFrom(table, rel)
		where := RowFilter{Where: Eq(id.Expr(), Input("id", Int8()).Expr())}
		_, err := TryDefineDeleteMany(src, "remove", MutationSpec{RowFilter: where})
		switch {
		case spec.ReadOnly:
			require.ErrorIs(t, err, ErrReadOnly)
		case spec.AllowDelete:
			require.NoError(t, err)
			require.Len(t, table.Definition().Statements, 1)
		default:
			require.ErrorIs(t, err, ErrOperationDenied)
			out := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
			_, err = TryDefineRows(out, "search", src, RowsSpec{}, Project(Output(out, "id", Int8()), id.Expr()))
			require.ErrorIs(t, err, ErrOperationDenied)
			require.Empty(t, out.Definition().Statements)
		}
	}
}

type groupKey struct {
	GroupID int64 `db:"group_id,key"`
	ItemID  int64 `db:"item_id,key"`
}

func TestPlanSourceCompositeKeyLock(t *testing.T) {
	for _, mode := range []WriteMode{WriteDefault, WriteUpdateOnly} {
		reg := NewRegistry(RegistryConfig[struct{}]{Deny: DenyLegacySQL})
		table := DefinePlanSource(reg, TableSpec[groupKey, groupKey]{Name: "public.groups", RelationScope: Global(), WriteMode: mode})
		rel := FromTable(table, "g", Global())
		id := FieldOf(rel, "item_id", Int8())
		out := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
		DefineRows(out, "hold", RowsFrom(table, rel), RowsSpec{Lock: ForRows(ForUpdate, rel, NoWait)},
			Project(Output(out, "id", Int8()), id.Expr()))
		d := out.Definition()
		require.Len(t, d.Statements, 1)
		require.Contains(t, d.Statements["hold.global.data"], `FOR UPDATE OF "g" NOWAIT`)
		require.Equal(t, []string{`"public"."groups"`}, d.StatementShapes["hold.global.data"].AnyColumnUpdate)
		require.Equal(t, []DefinedPrivilege{{Table: `"public"."groups"`, Privilege: "SELECT", Columns: []string{"item_id"}}}, d.StatementShapes["hold.global.data"].Privileges)
		require.Empty(t, table.Definition().Statements)
	}
}
