package sqlcheck

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck/testdata/writeplans"
	"github.com/stretchr/testify/require"
)

func TestCollectWritePlanShapes(t *testing.T) {
	inv, err := CollectSQL(t.Context(), InventoryConfig{Dir: "testdata/writeplans", Patterns: []string{"."}})
	require.NoError(t, err)
	require.Empty(t, inv.Findings)
	tables, views := writeplans.Definitions()
	snapshot := RenderAllSnapshot(tables, views)
	defs, err := ParseSnapshot(snapshot)
	require.NoError(t, err)
	require.Equal(t, snapshot, RenderSnapshot(defs))
	counts := map[string]int{"insert_count": 1, "insert_batch": 2, "insert_result": 1, "insert_results": 2,
		"mapped": 1, "change": 2, "remove": 2, "graph": 1}
	bindings := make(map[string]SQLBinding)
	methods := make(map[string]bool)
	var indirect []SQLSite
	for _, site := range inv.Sites {
		require.Equal(t, "typed_rows", site.Kind)
		require.Empty(t, site.SQL)
		require.False(t, site.Dynamic)
		methods[site.Method] = true
		b := SQLBinding{Roles: []string{site.ID}}
		if site.QueryKey == "" {
			require.Empty(t, site.Definition)
			indirect = append(indirect, site)
			if site.Method == "Exec" {
				b.Definition, b.Plan = "public.items", "insert_count"
			} else {
				require.Equal(t, "All", site.Method)
				b.Definition, b.Plan = "entries", "insert_result"
			}
		} else {
			require.Contains(t, counts, site.QueryKey)
			wantOwner := "entries"
			if site.QueryKey == "insert_count" || site.QueryKey == "insert_batch" || site.QueryKey == "mapped" {
				wantOwner = "public.items"
			}
			require.Equal(t, wantOwner, site.Definition)
		}
		bindings[site.ID] = b
	}
	require.Len(t, inv.Sites, 21)
	require.Len(t, indirect, 2)
	require.ElementsMatch(t, []string{"TryDefineWrite", "DefineWrite", "DefineReturning", "TryDefineReturning", "TryDefineTableReturning",
		"TryDefineUpdateReturning", "DefineDeleteReturning", "Exec", "ExecBatch", "All", "Iter", "AllBatch",
		"UpdateMany", "UpdateIter", "DeleteMany", "DeleteIter"}, slices.Collect(maps.Keys(methods)))
	work, err := inv.BindRolesWithDefinitions(bindings, defs, nil)
	require.NoError(t, err)
	direct, err := inv.BindRolesWithDefinitions(bindings, tables, views)
	require.NoError(t, err)
	require.Equal(t, direct, work)
	for _, site := range inv.Sites {
		plan := site.QueryKey
		if plan == "" {
			plan = bindings[site.ID].Plan
		}
		require.Len(t, work[site.ID], counts[plan])
		for _, statement := range work[site.ID] {
			require.Contains(t, statement.Name, "/"+plan+".rls.")
			require.NotNil(t, statement.Shape)
			require.NotZero(t, statement.Shape.Effects&tabula.EffectWrite)
			require.NotEmpty(t, statement.Access)
			require.NotContains(t, statement.Name, ".list.")
			require.NotContains(t, statement.Name, ".data")
		}
		needs, err := work.Readiness(site.ID)
		require.NoError(t, err)
		if plan == "graph" {
			require.Equal(t, []string{`"public"."items"`, `"public"."notes"`}, needs.ForceRLS)
			require.Equal(t, map[string]bool{"id": true}, needs.Privileges.Columns[`"public"."items"`]["SELECT"])
			require.Equal(t, map[string]bool{"id": true, "rank": true}, needs.Privileges.Columns[`"public"."notes"`]["INSERT"])
			require.Empty(t, needs.Privileges.Columns[`"public"."notes"`]["SELECT"])
			require.ElementsMatch(t, []PolicyAccess{{`"public"."items"`, "SELECT"}, {`"public"."notes"`, "INSERT"}}, work[site.ID][0].Access)
		} else {
			require.Equal(t, []string{`"public"."items"`}, needs.ForceRLS)
		}
		if strings.HasPrefix(plan, "insert") {
			require.Equal(t, map[string]bool{"id": true, "rank": true}, needs.Privileges.Columns[`"public"."items"`]["INSERT"])
		}
	}
	for _, site := range indirect {
		_, err := (SQLInventory{Sites: []SQLSite{site}}).BindRolesWithDefinitions(map[string]SQLBinding{site.ID: {Roles: []string{"writer"}}}, defs, nil)
		require.ErrorContains(t, err, "specify Definition and Plan")
	}
}

func TestWritePlanBindingRejectsMismatchedForms(t *testing.T) {
	tables, views := writeplans.Definitions()
	for _, tc := range []struct{ receiver, method, owner, plan string }{
		{"BoundWritePlan", "Exec", "public.items", "insert_batch"},
		{"BoundWritePlan", "ExecBatch", "public.items", "insert_count"},
		{"BoundWritePlan", "Exec", "entries", "insert_result"},
		{"BoundReturningPlan", "All", "public.items", "insert_count"},
		{"BoundReturningPlan", "AllBatch", "entries", "insert_result"},
		{"BoundReturningPlan", "Iter", "entries", "insert_results"},
		{"BoundMutationReturningPlan", "UpdateMany", "entries", "remove"},
		{"BoundMutationReturningPlan", "DeleteIter", "entries", "change"},
	} {
		t.Run(tc.method+"/"+tc.plan, func(t *testing.T) {
			site := SQLSite{ID: "use", Kind: "typed_rows", Receiver: tabulaPkgPath + "." + tc.receiver,
				Method: tc.method, Definition: tc.owner, QueryKey: tc.plan}
			bindings := map[string]SQLBinding{"use": {Roles: []string{"writer"}}}
			_, err := (SQLInventory{Sites: []SQLSite{site}}).BindRolesWithDefinitions(bindings, tables, views)
			require.ErrorContains(t, err, "no matching typed shapes")
		})
	}
}

func TestWriteShapeKeys(t *testing.T) {
	shape := tabula.StatementShape{Effects: tabula.EffectWrite, Result: []tabula.ShapeField{{Name: "id"}}}
	for _, tc := range []struct {
		method, tail string
		valid        bool
	}{
		{"All", "write", true}, {"All", "write.", false}, {"AllBatch", "rows.3", true},
		{"AllBatch", "rows.03", false}, {"AllBatch", "rows.0", false}, {"AllBatch", "rows.-1", false},
		{"AllBatch", "rows.3.where.active", false}, {"UpdateIter", "set.rank", true},
		{"UpdateIter", "set.rank.where.first+second", true}, {"UpdateIter", "set.rank.", false},
		{"UpdateIter", "set.rank.where.first+first", false}, {"UpdateIter", "set..where.first", false},
		{"DeleteIter", "delete", true}, {"DeleteIter", "delete.where.first", true},
		{"DeleteIter", "delete.", false}, {"DeleteIter", "delete.where.", false},
	} {
		site := SQLSite{Method: tc.method}
		key := "change.global." + tc.tail
		require.Equal(t, tc.valid, site.matchesWriteShape(key, "change", shape), key)
		require.False(t, site.matchesWriteShape(key, "other", shape))
		require.False(t, site.matchesWriteShape("change.other."+tc.tail, "change", shape))
		read := shape
		read.Effects = tabula.EffectRead
		require.False(t, site.matchesWriteShape(key, "change", read))
	}
}
