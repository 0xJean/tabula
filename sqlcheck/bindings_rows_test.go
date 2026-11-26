package sqlcheck

import (
	"slices"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck/testdata/rowplans"
	"github.com/stretchr/testify/require"
)

func TestRowInventoryRoleBinding(t *testing.T) {
	inv, err := CollectSQL(t.Context(), InventoryConfig{Dir: "testdata/rowplans", Patterns: []string{"."}})
	require.NoError(t, err)
	require.Empty(t, inv.Findings)
	require.Len(t, inv.Sites, 8)
	bind := make(map[string]SQLBinding)
	var countSite, unknown SQLSite
	for _, site := range inv.Sites {
		b := SQLBinding{Roles: []string{"reader"}}
		switch site.Method {
		case "DefineRows", "Count", "List":
			require.Equal(t, "entries", site.Definition)
			require.Equal(t, "items_by_rank", site.QueryKey)
			if site.Method == "Count" {
				countSite = site
				b.Roles = []string{"counter"}
			}
		case "TryDefineUpdateMany", "UpdateMany":
			require.Equal(t, "public.items", site.Definition)
			require.Equal(t, "change_rank", site.QueryKey)
			b.Roles = []string{"editor"}
		case "DefineDeleteMany", "DeleteMany":
			require.Equal(t, "public.items", site.Definition)
			require.Equal(t, "remove_items", site.QueryKey)
			b.Roles = []string{"remover"}
		case "Find":
			unknown = site
			require.Empty(t, site.QueryKey)
			b.Definition, b.Plan, b.Prefix = "entries", "items_by_rank", "items_by_rank.global.data.sort.forward"
		default:
			t.Fatalf("unexpected method %q", site.Method)
		}
		require.Equal(t, "typed_rows", site.Kind)
		bind[site.ID] = b
	}
	tables, views := rowplans.Definitions()
	snap := RenderAllSnapshot(tables, views)
	for range 8 {
		require.Equal(t, snap, RenderAllSnapshot(tables, views))
	}
	defs, err := ParseSnapshot(snap)
	require.NoError(t, err)
	require.Equal(t, snap, RenderSnapshot(defs))
	work, err := inv.BindRolesWithDefinitions(bind, defs, nil)
	require.NoError(t, err)
	require.Len(t, work["reader"], 7)
	require.Len(t, work["counter"], 1)
	stmt := work["counter"][0]
	require.NotNil(t, stmt.Shape)
	require.Equal(t, []PolicyAccess{{`"public"."items"`, "SELECT"}}, stmt.Access)
	require.NotContains(t, stmt.SQL, "ORDER BY")
	require.Contains(t, stmt.Name, countSite.ID)
	require.Equal(t, tabula.EffectRead, stmt.Shape.Effects)
	needs, err := work.Readiness("counter")
	require.NoError(t, err)
	require.Equal(t, []string{"pg_catalog", "public"}, needs.SchemaUsage)
	require.Equal(t, map[string]bool{"rank": true}, needs.Privileges.Columns[`"public"."items"`]["SELECT"])
	needs, err = work.Readiness("editor")
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"rank": true}, needs.Privileges.Columns[`"public"."items"`]["UPDATE"])
	require.Empty(t, needs.Privileges.TableDelete)
	_, err = work.Readiness("unknown")
	require.Error(t, err)
	require.Len(t, work["remover"], 2)
	direct, err := inv.BindRolesWithDefinitions(bind, tables, views)
	require.NoError(t, err)
	require.Equal(t, work, direct)

	for _, b := range []SQLBinding{
		{Plan: "wrong"}, {Definition: "wrong"}, {NoAccess: true},
		{Prefix: "items_by_rank.global.data"}, {Variants: []Statement{{Name: "override", SQL: "SELECT 1"}}},
	} {
		b.Roles = []string{"counter"}
		_, err := (SQLInventory{Sites: []SQLSite{countSite}}).BindRolesWithDefinitions(map[string]SQLBinding{countSite.ID: b}, defs, nil)
		require.Error(t, err)
	}
	bind[unknown.ID] = SQLBinding{Roles: []string{"reader"}}
	_, err = inv.BindRolesWithDefinitions(bind, defs, nil)
	require.ErrorContains(t, err, "specify Definition and Plan")
	bind[unknown.ID] = SQLBinding{Roles: []string{"reader"}, Definition: "entries", Plan: "items_by_rank"}
	i := slices.IndexFunc(defs, func(d tabula.Definition) bool { return d.Name == "entries" })
	delete(defs[i].StatementShapes, "items_by_rank.global.count")
	_, err = inv.BindRolesWithDefinitions(bind, defs, nil)
	require.ErrorContains(t, err, "metadata")
}

func TestRowWorkloadPolicyNeeds(t *testing.T) {
	const table = `"public"."items"`
	d := tabula.Definition{Statements: map[string]string{"lock": "SELECT 1", "routine": "SELECT 2"},
		StatementShapes: map[string]tabula.StatementShape{
			"lock":    {Effects: tabula.EffectRead | tabula.EffectRowLock, Access: []tabula.ShapeAccess{{Relation: table, Command: "SELECT"}}, AnyColumnUpdate: []string{table}},
			"routine": {Effects: tabula.EffectRead},
		}}
	lock := definitionStatement(d, "lock", "read_lock")
	require.Equal(t, []PolicyAccess{{table, "SELECT"}, {table, "UPDATE"}}, lock.Access)
	require.Equal(t, []tabula.ShapeAccess{{Relation: table, Command: "SELECT"}}, lock.Shape.Access)
	routine := definitionStatement(d, "routine", "read_routine")
	require.False(t, routine.NoAccess)
	key := "evaluate.global.data"
	d = tabula.Definition{Name: "entries", Statements: map[string]string{key: "SELECT public.evaluate()"},
		StatementShapes: map[string]tabula.StatementShape{key: {Effects: tabula.EffectRead}}, StatementQueries: map[string]string{key: "evaluate"}}
	inv := SQLInventory{Sites: []SQLSite{{ID: "call", Kind: "typed_rows", Method: "Find", Definition: d.Name, QueryKey: "evaluate"}}}
	for _, b := range []SQLBinding{{}, {Access: []PolicyAccess{{Table: table, Command: "SELECT"}}}, {NoAccess: true}} {
		b.Roles = []string{"reader"}
		work, err := inv.BindRolesWithDefinitions(map[string]SQLBinding{"call": b}, []tabula.Definition{d}, nil)
		if len(b.Access) == 0 && !b.NoAccess {
			require.ErrorContains(t, err, "reviewed Access or NoAccess")
			continue
		}
		require.NoError(t, err)
		require.Equal(t, b.NoAccess, work["reader"][0].NoAccess)
		require.Equal(t, b.Access, work["reader"][0].Access)
	}
}

func TestListPairsCountForSelectedData(t *testing.T) {
	site := SQLSite{ID: "page", Method: "List", Kind: "typed_rows", Definition: "entries", QueryKey: "search"}
	d := tabula.Definition{Name: "entries", Statements: map[string]string{}, StatementShapes: map[string]tabula.StatementShape{}, StatementQueries: map[string]string{}}
	for _, k := range []string{"search.global.data.sort.where.offset.where.label", "search.global.count.where.label",
		"search.global.data.sort.where.offset", "search.global.count"} {
		d.Statements[k] = "SELECT id FROM public.items"
		d.StatementQueries[k] = "search"
		d.StatementShapes[k] = tabula.StatementShape{Effects: tabula.EffectRead, Access: []tabula.ShapeAccess{{Relation: "public.items", Command: "SELECT"}}}
	}
	inv := SQLInventory{Sites: []SQLSite{site}}
	bindings := map[string]SQLBinding{"page": {Roles: []string{"reader"}, Prefix: "search.global.data.sort.where.offset.where.label"}}
	work, err := inv.BindRolesWithDefinitions(bindings, []tabula.Definition{d}, nil)
	require.NoError(t, err)
	require.Len(t, work["reader"], 2)
	require.Equal(t, "page/entries/search.global.count.where.label", work["reader"][0].Name)
	require.Equal(t, "search.global.count", pairedRowCount("search.global.data.sort.where.offset", "search"))
}

func TestTypedAggregatePrivilegeInventory(t *testing.T) {
	tables, views := rowplans.Definitions()
	all := append(tables, ViewsToDefinitions(views)...)
	needs, empty := CollectPrivilegeNeeds(all)
	require.Empty(t, empty)
	require.Len(t, needs.Columns, 1)
	require.True(t, needs.Columns["public.items"]["SELECT"]["id"])
	needs, empty = CollectPrivilegeNeeds(ViewsToDefinitions(views))
	require.Empty(t, empty)
	require.Equal(t, map[string]bool{"rank": true, "id": true}, needs.Columns[`"public"."items"`]["SELECT"])
}
