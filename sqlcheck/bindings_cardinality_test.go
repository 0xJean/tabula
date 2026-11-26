package sqlcheck

import (
	"strings"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck/testdata/mappedreads"
	"github.com/stretchr/testify/require"
)

func TestMappedReadInventory(t *testing.T) {
	inventory, err := CollectSQL(t.Context(), InventoryConfig{Dir: "testdata/mappedreads", Patterns: []string{"."}})
	require.NoError(t, err)
	require.Empty(t, inventory.Findings)
	tables, views := mappedreads.Definitions()
	snapshot := RenderAllSnapshot(tables, views)
	defs, err := ParseSnapshot(snapshot)
	require.NoError(t, err)
	require.Equal(t, snapshot, RenderSnapshot(defs))

	bind := make(map[string]SQLBinding)
	for _, site := range inventory.Sites {
		owner := "entries"
		if site.QueryKey == "lookup" || site.QueryKey == "visible" || site.QueryKey == "brief" {
			owner = "public.items"
		}
		require.Equal(t, owner, site.Definition)
		require.Equal(t, "typed_rows", site.Kind)
		bind[site.ID] = SQLBinding{Roles: []string{site.QueryKey + "/" + site.Method}}
	}
	work, err := inventory.BindRolesWithDefinitions(bind, defs, nil)
	require.NoError(t, err)
	want := map[string][]string{
		"brief/TryDefineTableRead": {"data", "list.count", "list.count.where.first", "list.data.sort.offset.offset", "list.data.sort.offset.offset.where.first"},
		"brief/List":               {"list.count", "list.count.where.first", "list.data.sort.offset.offset", "list.data.sort.offset.offset.where.first"},
		"brief/Page":               {"list.data.sort.offset.offset", "list.data.sort.offset.offset.where.first"},
		"brief/Count":              {"list.count", "list.count.where.first"},
		"brief/Find":               {"data"},
		"lookup/One":               {"single_first.sort.offset", "single_first.sort.offset.where.first"},
		"lookup/MaybeFirst":        {"single_first.sort.offset", "single_first.sort.offset.where.first"},
		"visible/DefineExists":     {"exists"},
		"visible/Exists":           {"exists"},
		"unlocked/MaybeOne":        {"data.sort.offset", "data.sort.offset.where.first"},
		"unlocked/First":           {"firstrow.sort.offset", "firstrow.sort.offset.where.first"},
		"joined/TryDefineRead":     {"data", "single", "list.count", "list.data.sort.offset.offset"},
		"joined/Page":              {"list.data.sort.offset.offset"},
		"joined/List":              {"list.count", "list.data.sort.offset.offset"},
		"joined/Count":             {"list.count"},
		"joined/One":               {"single"},
	}
	for role, forms := range want {
		var got []string
		for _, stmt := range work[role] {
			_, suffix, ok := strings.Cut(stmt.Name, ".rls.")
			require.True(t, ok)
			got = append(got, suffix)
		}
		require.ElementsMatch(t, forms, got, role)
	}
	require.Len(t, work["lookup/TryDefineTableRows"], 8)
	require.Len(t, work["unlocked/DefineRowsWithForms"], 8)
	require.Len(t, work, len(want)+2)
	needs, err := work.Readiness("lookup/One")
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"id": true}, needs.Privileges.Columns[`"public"."items"`]["SELECT"])
	needs, err = work.Readiness("visible/Exists")
	require.NoError(t, err)
	require.Equal(t, []string{`"public"."items"`}, needs.AnyColumnSelect)
	require.Equal(t, needs.AnyColumnSelect, needs.ForceRLS)
	needs, err = work.Readiness("brief/Count")
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"id": true}, needs.Privileges.Columns[`"public"."items"`]["SELECT"])
	needs, err = work.Readiness("brief/Page")
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"id": true, "note": true}, needs.Privileges.Columns[`"public"."items"`]["SELECT"])
}

func TestExactShapeFallback(t *testing.T) {
	const plan = "look"
	data := "look.global.data.sort.where.where.offset"
	d := tabula.Definition{StatementQueries: map[string]string{}}
	shape := tabula.StatementShape{Effects: tabula.EffectRead}
	require.True(t, matchesSingleRow(data, plan, "One", shape, d))
	require.False(t, matchesSingleRow("look.global.data.sort.where.first", plan, "MaybeOne", shape, d))
	require.False(t, matchesSingleRow(data, plan, "MaybeFirst", shape, d))
	d.StatementQueries["look.global.single_first.sort.where.where.offset"] = plan
	require.False(t, matchesSingleRow(data, plan, "One", shape, d))
	shape.Effects |= tabula.EffectRowLock
	require.False(t, matchesSingleRow("look.global.data", plan, "One", shape, d))
}
