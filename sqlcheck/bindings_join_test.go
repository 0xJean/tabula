package sqlcheck

import (
	"testing"

	"github.com/0xJean/tabula/sqlcheck/testdata/joinplans"
	"github.com/stretchr/testify/require"
)

func TestJoinedReadInventory(t *testing.T) {
	inventory, err := CollectSQL(t.Context(), InventoryConfig{Dir: "testdata/joinplans", Patterns: []string{"."}})
	require.NoError(t, err)
	require.Empty(t, inventory.Findings)
	require.Len(t, inventory.Sites, 2)
	bind := map[string]SQLBinding{}
	var methods []string
	for _, site := range inventory.Sites {
		require.Equal(t, "entries", site.Definition)
		require.Equal(t, "match", site.QueryKey)
		methods = append(methods, site.Method)
		bind[site.ID] = SQLBinding{Roles: []string{"scope_reader"}}
	}
	require.ElementsMatch(t, []string{"TryDefineRead", "Find"}, methods)
	defs, err := ParseSnapshot(RenderAllSnapshot(nil, joinplans.Definitions()))
	require.NoError(t, err)
	work, err := inventory.BindRolesWithDefinitions(bind, defs, nil)
	require.NoError(t, err)
	for _, s := range work["scope_reader"] {
		require.Equal(t, []PolicyAccess{{`"public"."items"`, "SELECT"}, {`"public"."notes"`, "SELECT"}}, s.Access)
	}
	needs, err := work.Readiness("scope_reader")
	require.NoError(t, err)
	require.Equal(t, []string{`"public"."items"`}, needs.ForceRLS)
	for _, table := range []string{`"public"."items"`, `"public"."notes"`} {
		require.Equal(t, map[string]bool{"id": true}, needs.Privileges.Columns[table]["SELECT"])
	}
}
