package sqlcheck

import (
	"slices"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck/testdata/effectplans"
	"github.com/stretchr/testify/require"
)

func TestCollectLockAndRoutineShapes(t *testing.T) {
	inv, err := CollectSQL(t.Context(), InventoryConfig{Dir: "testdata/effectplans", Patterns: []string{"."}})
	require.NoError(t, err)
	require.Empty(t, inv.Findings)
	require.Len(t, inv.Sites, 11)
	tables, views := effectplans.Definitions()
	defs, err := ParseSnapshot(RenderAllSnapshot(tables, views))
	require.NoError(t, err)
	const table = `"public"."items"`
	bindings := make(map[string]SQLBinding)
	var indirect, advisory, tuple SQLSite
	for _, site := range inv.Sites {
		if site.QueryKey == "select_items" {
			require.Equal(t, "typed_rows", site.Kind)
		} else {
			require.Equal(t, "typed_plan", site.Kind)
		}
		b := SQLBinding{Roles: []string{"reader"}}
		switch site.QueryKey {
		case "hold":
			require.Equal(t, "notes", site.Definition)
			if site.Method == "Acquire" {
				advisory = site
			}
		case "select_items":
			require.Equal(t, "entries", site.Definition)
		case "count_items":
			require.Equal(t, "entries", site.Definition)
			b.Access = []PolicyAccess{{Table: table, Command: "SELECT"}}
		case "", "change_items":
			if site.Method == "TryDefineAdvisoryLock" {
				tuple = site
				require.Empty(t, site.Definition)
				b.Definition, b.Plan = "notes", "hold"
				break
			}
			if site.QueryKey == "" {
				require.Equal(t, "All", site.Method)
				require.Equal(t, tabulaPkgPath+".BoundRoutinePlan", site.Receiver)
				require.Empty(t, site.Definition)
				indirect = site
				b.Definition, b.Plan = "entries", "change_items"
			}
			b.Roles = []string{"editor"}
			b.Access = []PolicyAccess{{Table: table, Command: "UPDATE"}}
		default:
			t.Fatalf("unexpected collected plan %q", site.QueryKey)
		}
		bindings[site.ID] = b
	}
	work, err := inv.BindRolesWithDefinitions(bindings, defs, nil)
	require.NoError(t, err)
	direct, err := inv.BindRolesWithDefinitions(bindings, tables, views)
	require.NoError(t, err)
	require.Equal(t, direct, work)
	require.Len(t, work["reader"], 8)
	require.Len(t, work["editor"], 3)
	for _, s := range work["reader"] {
		switch s.Shape.Effects {
		case tabula.EffectRead | tabula.EffectRowLock:
			require.ElementsMatch(t, []PolicyAccess{{Table: table, Command: "SELECT"}, {Table: table, Command: "UPDATE"}}, s.Access)
		case tabula.EffectRead | tabula.EffectAdvisoryLock:
			require.Empty(t, s.Access)
			require.Empty(t, s.Shape.Routines)
		case tabula.EffectRead:
			require.Equal(t, []PolicyAccess{{Table: table, Command: "SELECT"}}, s.Access)
			require.False(t, s.Shape.Routines[0].ReturnsSet)
		default:
			t.Fatalf("unexpected reader effects %d", s.Shape.Effects)
		}
	}
	for _, s := range work["editor"] {
		require.Equal(t, tabula.EffectWrite, s.Shape.Effects)
		require.Equal(t, []PolicyAccess{{Table: table, Command: "UPDATE"}}, s.Access)
		require.True(t, s.Shape.Routines[0].ReturnsSet)
	}
	needs, err := work.Readiness("reader")
	require.NoError(t, err)
	require.Equal(t, []string{table}, needs.AnyColumnUpdate)
	require.True(t, slices.ContainsFunc(needs.Routines, func(r tabula.RoutineRequirement) bool {
		return r.Signature == `"public"."item_count"()` && r.ReadOnly && !r.ReturnsSet
	}))

	require.NotEmpty(t, tuple.ID)
	require.NotEmpty(t, advisory.ID)
	require.NotEmpty(t, indirect.ID)
	wrong := func(site SQLSite, owner, plan string) {
		t.Helper()
		site.Definition, site.QueryKey = "", ""
		b := SQLBinding{Roles: []string{"reader"}, Definition: owner, Plan: plan}
		_, err := (SQLInventory{Sites: []SQLSite{site}}).BindRolesWithDefinitions(map[string]SQLBinding{site.ID: b}, defs, nil)
		require.ErrorContains(t, err, "no matching typed shapes")
	}
	wrong(advisory, "entries", "select_items")
	wrong(indirect, "entries", "select_items")
	wrong(indirect, "entries", "count_items")
}
