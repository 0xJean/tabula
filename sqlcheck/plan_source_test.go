package sqlcheck

import (
	"encoding/json"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck/testdata/plansources"
	"github.com/stretchr/testify/require"
)

func TestPlanSourceInventory(t *testing.T) {
	d := plansources.Definitions()
	needs, empty := CollectPrivilegeNeeds(d)
	require.Empty(t, empty)
	require.Equal(t, map[string]map[string]map[string]bool{
		"public.items":  {"SELECT": {"id": true}, "UPDATE": {"rank": true}},
		"public.groups": {"SELECT": {"id": true}},
	}, needs.Columns)
	require.Equal(t, map[string]bool{"public.groups": true}, needs.TableDelete)
	require.Empty(t, needs.Sequences)
	snapshot := RenderSnapshot(d)
	parsed, err := ParseSnapshot(snapshot)
	require.NoError(t, err)
	require.Equal(t, snapshot, RenderSnapshot(parsed))
	inv, err := CollectSQL(t.Context(), InventoryConfig{Dir: "testdata/plansources", Patterns: []string{"."}})
	require.NoError(t, err)
	require.Empty(t, inv.Findings)
	require.Len(t, inv.Sites, 4)
	bindings := make(map[string]SQLBinding)
	for _, site := range inv.Sites {
		switch site.QueryKey {
		case "change":
			require.Equal(t, "public.items", site.Definition)
			bindings[site.ID] = SQLBinding{Roles: []string{"scope_writer"}}
		case "remove":
			require.Equal(t, "public.groups", site.Definition)
			bindings[site.ID] = SQLBinding{Roles: []string{"scope_owner"}}
		default:
			t.Fatalf("unexpected site %+v", site)
		}
	}
	work, err := inv.BindRolesWithDefinitions(bindings, parsed, nil)
	require.NoError(t, err)
	r, err := work.Readiness("scope_writer")
	require.NoError(t, err)
	require.Equal(t, []string{`"public"."items"`}, r.ForceRLS)
	require.Equal(t, map[string]map[string]map[string]bool{`"public"."items"`: {"SELECT": {"id": true}, "UPDATE": {"rank": true}}}, r.Privileges.Columns)
	require.Empty(t, r.Privileges.TableDelete)
}

func TestPlanSourceWithoutStatements(t *testing.T) {
	type key struct {
		ID int64 `db:"id,key"`
	}
	r := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	s := tabula.DefinePlanSource(r, tabula.TableSpec[key, int64]{Name: "public.items", RelationScope: tabula.Global()})
	encoded, err := json.Marshal(s.Definition())
	require.NoError(t, err)
	var source tabula.Definition
	require.NoError(t, json.Unmarshal(encoded, &source))
	needs, empty := CollectPrivilegeNeeds([]tabula.Definition{source, {Name: "undeclared"}, {Name: "projection", Privileges: []tabula.DefinedPrivilege{}}})
	require.Empty(t, needs.Columns)
	require.Equal(t, []string{"undeclared", "projection"}, empty)
	require.Empty(t, RenderSnapshot(r.Defined()))
}
