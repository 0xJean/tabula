package tabula

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShapePlanNameBelongsToOneDeclaration(t *testing.T) {
	reg := NewRegistry(RegistryConfig[struct{}]{})
	view := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	shape := func(key string) *compiledShape {
		return &compiledShape{key: key, sql: "SELECT 1", layout: &parameterLayout{}, shape: StatementShape{Effects: EffectRead}}
	}
	first, second := shape("change.first"), shape("change.second")
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, s := range []*compiledShape{first, second} {
		wg.Go(func() { <-start; results <- view.registerShapes("change", []*compiledShape{s}) })
	}
	close(start)
	wg.Wait()
	close(results)
	var successes int
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		require.ErrorContains(t, err, `plan "change" already registered`)
	}
	require.Equal(t, 1, successes)
	snapshot := view.Definition()
	require.Len(t, snapshot.StatementQueries, 1)
	require.Len(t, snapshot.StatementShapes, 1)
	require.NotEqual(t, first.authority == nil, second.authority == nil)
	require.Panics(t, func() { view.registerStatement("change.third", "SELECT 3", "change", nil) })
	require.Equal(t, snapshot, view.Definition())

	other := DefineView(reg, ViewSpec[typedResult]{Name: "notes"})
	require.NoError(t, other.registerShapes("change", []*compiledShape{shape("change.first")}))
	other.registerStatement("read.first", "SELECT 1", "read", nil)
	require.ErrorContains(t, other.registerShapes("read", []*compiledShape{shape("read.other")}), "already registered")
}

func TestStrictDescriptorDeclarations(t *testing.T) {
	reg := NewRegistry(RegistryConfig[struct{}]{Deny: DenyLegacySQL})
	require.True(t, reg.SQLAuthority().DeniesLegacySQL())
	require.False(t, SQLAuthority{}.DeniesLegacySQL())
	require.ErrorIs(t, ErrLegacySQL, ErrCapabilityDenied)
	spec := TableSpec[typedItem, int64]{Name: "public.items", Order: "LABEL ASC NULLS LAST, id DESC"}
	tab := Define(reg, spec)
	one := tab.Definition().Statements["get"]
	require.PanicsWithValue(t, ErrLegacySQL, func() { tab.Statement("read_again", one) })
	view := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	_, err := view.TryDefineQuery("listed", QuerySpec{Select: "SELECT 1"})
	require.ErrorIs(t, err, ErrLegacySQL)
	require.PanicsWithValue(t, ErrLegacySQL, func() { view.Statement("find", "SELECT 1") })
	_, err = tab.TryDefineUpsert("update_item", UpsertSpec{Conflict: []string{"id"}, Update: []string{"label"}, Where: "true"})
	require.ErrorIs(t, err, ErrLegacySQL)
	require.NotPanics(t, func() {
		tab.DefineUpsert("update_item", UpsertSpec{Conflict: []string{"id"}, Update: []string{"label"}})
	})
	for _, order := range []string{"lower(label)", "id,", "id NULLS", "absent DESC", "id DESC NULLS LAST, label + 1"} {
		spec.Order = order
		_, err := TryDefine(reg, spec)
		require.ErrorIs(t, err, ErrLegacySQL)
	}
	spec.Order = ""
	spec.Name = "items AS i"
	_, err = TryDefine(reg, spec)
	require.ErrorIs(t, err, ErrLegacySQL)
	type badColumn struct {
		ID   int64 `db:"id,key"`
		Rank int64 `db:"rank + 1"`
	}
	_, err = TryDefine(reg, TableSpec[badColumn, int64]{Name: "public.items"})
	require.ErrorIs(t, err, ErrLegacySQL)
}

func TestTypedColumnLengthAndSecretProjection(t *testing.T) {
	reg := NewRegistry(RegistryConfig[struct{}]{})
	tab := Define(reg, TableSpec[typedItem, int64]{Name: "public.items", RelationScope: Global()})
	rel := FromTable(tab, "i", Global())
	type projected struct {
		Label string `db:"label,secret"`
	}
	view := DefineView(reg, ViewSpec[projected]{Name: "notes"})
	c, err := newTypedCompiler(false, rel.source)
	require.NoError(t, err)
	length := Varchar(8)
	slot := Input("label", length)
	fields, result, err := c.project(view.entry, []Projection{Project(Output(view, "label", length), slot.Expr())})
	require.NoError(t, err)
	require.True(t, c.hasSecret)
	require.Equal(t, []string{`CAST($1 AS "pg_catalog"."varchar") AS "label"`}, fields)
	read := func(typ PGType[string]) error { return c.read(FieldOf(rel, "label", typ).ref) }
	require.NoError(t, read(CatalogType[string]("pg_catalog", "varchar", nil)))
	require.NoError(t, read(length))
	require.NoError(t, read(CatalogType[string]("pg_catalog", "varchar", nil)))
	require.ErrorContains(t, read(Varchar(12)), "conflicting PostgreSQL types")
	meta, err := c.readRequirements()
	require.NoError(t, err)
	meta.Result = result
	shape, err := c.finishShape("labels", "SELECT "+fields[0]+" FROM public.items", catalogType{}, meta)
	require.NoError(t, err)
	require.True(t, shape.shape.Inputs[0].Secret)
	require.NoError(t, view.registerShapes("labels", []*compiledShape{shape}))
	before := view.Definition().StatementShapes["labels"].PhysicalColumns[0]
	require.NotNil(t, before.TypeMod)
	require.EqualValues(t, 12, *before.TypeMod)
	*before.TypeMod = 99
	require.EqualValues(t, 12, *view.Definition().StatementShapes["labels"].PhysicalColumns[0].TypeMod)
	for _, n := range []int{-1, 0, 10_485_761} {
		require.Panics(t, func() { Varchar(n) })
	}
}
