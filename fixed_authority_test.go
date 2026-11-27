package tabula

import (
	"sync"
	"testing"

	"github.com/0xJean/tabula/internal/planrt"
	"github.com/stretchr/testify/require"
)

type strictItem struct {
	ID    int64  `db:"id,key"`
	Label string `db:"label"`
}

type strictResult struct {
	ID int64 `db:"id"`
}

func TestShapePlanNameBelongsToOneDeclaration(t *testing.T) {
	reg := NewRegistry(RegistryConfig[struct{}]{})
	view := DefineView(reg, ViewSpec[strictResult]{Name: "entries"})
	shape := func(key string) planrt.ShapeRegistration {
		return planrt.ShapeRegistration{
			Compiled: &planrt.CompiledShape{Key: key, SQL: "SELECT 1"},
			Shape:    StatementShape{Effects: EffectRead},
			Check:    func(*planrt.Issuer) error { return nil },
		}
	}
	first, second := shape("change.first"), shape("change.second")
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, s := range []planrt.ShapeRegistration{first, second} {
		wg.Go(func() { <-start; results <- view.registerPlanShapes("change", []planrt.ShapeRegistration{s}) })
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
	require.NotEqual(t, first.Compiled.Authority == nil, second.Compiled.Authority == nil)
	require.Panics(t, func() { view.registerStatement("change.third", "SELECT 3", "change", nil) })
	require.Equal(t, snapshot, view.Definition())

	other := DefineView(reg, ViewSpec[strictResult]{Name: "notes"})
	require.NoError(t, other.registerPlanShapes("change", []planrt.ShapeRegistration{shape("change.first")}))
	other.registerStatement("read.first", "SELECT 1", "read", nil)
	require.ErrorContains(t, other.registerPlanShapes("read", []planrt.ShapeRegistration{shape("read.other")}), "already registered")
}

func TestStrictDescriptorDeclarations(t *testing.T) {
	reg := NewRegistry(RegistryConfig[struct{}]{Deny: DenyLegacySQL})
	require.True(t, reg.SQLAuthority().DeniesLegacySQL())
	require.False(t, SQLAuthority{}.DeniesLegacySQL())
	require.ErrorIs(t, ErrLegacySQL, ErrCapabilityDenied)
	spec := TableSpec[strictItem, int64]{Name: "public.items", Order: "LABEL ASC NULLS LAST, id DESC"}
	tab := Define(reg, spec)
	one := tab.Definition().Statements["get"]
	require.PanicsWithValue(t, ErrLegacySQL, func() { tab.Statement("read_again", one) })
	view := DefineView(reg, ViewSpec[strictResult]{Name: "entries"})
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
