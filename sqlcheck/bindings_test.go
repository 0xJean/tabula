package sqlcheck

import (
	"testing"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestInventoryDefinitionBinding(t *testing.T) {
	view := tabula.Definition{
		Name: "notes", Statements: map[string]string{"list.find": "SELECT 1", "list.page": "SELECT 2"},
		StatementQueries: map[string]string{"list.find": "list", "list.page": "list"},
	}
	access := []PolicyAccess{{Table: "public.notes", Command: "SELECT"}}
	base := SQLSite{ID: "view-site", Kind: "composed_query", QueryKey: "list", Definition: "notes"}
	bind := func(s SQLSite, b SQLBinding, defs ...tabula.Definition) (RoleStatements, error) {
		t.Helper()
		b.Roles = []string{"reader"}
		return (SQLInventory{Sites: []SQLSite{s}}).BindRolesWithDefinitions(map[string]SQLBinding{s.ID: b}, defs, nil)
	}

	work, err := bind(base, SQLBinding{Prefix: "list.page", Access: access}, view)
	require.NoError(t, err)
	require.Equal(t, []Statement{{Name: "view-site/notes/list.page", SQL: "SELECT 2", Access: access}}, work["reader"])
	work, err = bind(base, SQLBinding{Prefix: "list.page."}, view)
	require.NoError(t, err)
	require.Len(t, work["reader"], 1)
	_, err = bind(base, SQLBinding{Prefix: "other."}, view)
	require.ErrorContains(t, err, base.ID)
	_, err = bind(base, SQLBinding{Definition: "other"}, view)
	require.ErrorContains(t, err, "conflicts with source")
	_, err = bind(base, SQLBinding{}, view, view)
	require.ErrorContains(t, err, "ambiguous definition")
	unknown := base
	unknown.Definition = ""
	_, err = bind(unknown, SQLBinding{}, view)
	require.ErrorContains(t, err, "specify Definition")
	work, err = bind(unknown, SQLBinding{Definition: "notes"}, view)
	require.NoError(t, err)
	require.Len(t, work["reader"], 2)

	unregistered := base
	unregistered.Method = "TryDefineQuery"
	_, err = bind(unregistered, SQLBinding{}, view)
	require.ErrorContains(t, err, "TryDefineQuery")
	delete(view.Statements, "list.find")
	_, err = bind(base, SQLBinding{}, view)
	require.ErrorContains(t, err, "list.find")
	view.StatementQueries = nil // A snapshot from before query ownership was recorded.
	_, err = bind(base, SQLBinding{}, view)
	require.ErrorContains(t, err, "regenerate the snapshot")
}

func TestInventoryAggregateCoverage(t *testing.T) {
	const id = "group-site"
	def := tabula.Definition{Name: "notes", Statements: map[string]string{"count_by_kind": "SELECT 1"}}
	inv := SQLInventory{Sites: []SQLSite{
		{ID: "ping", SQL: "SELECT 1"},
		{ID: id, Kind: "aggregate", QueryKey: "countby:kind", Definition: "other"},
	}}
	bindings := map[string]SQLBinding{"ping": {Roles: []string{"reader"}}, id: {Roles: []string{"counter"}}}
	work, err := inv.BindRolesWithDefinitions(bindings, []tabula.Definition{def}, nil)
	require.Nil(t, work)
	require.ErrorContains(t, err, id)
	require.ErrorContains(t, err, "other")

	inv.Sites[1].Definition = def.Name
	inv.Sites[1].QueryKey = "countby:category"
	work, err = inv.BindRolesWithDefinitions(bindings, []tabula.Definition{def}, nil)
	require.Nil(t, work)
	require.ErrorContains(t, err, "count_by_category")
	inv.Sites[1].QueryKey = "max:rank"
	_, err = inv.BindRolesWithDefinitions(bindings, []tabula.Definition{def}, nil)
	require.ErrorContains(t, err, "named Variants")
	bindings[id] = SQLBinding{Skip: true}
	_, err = inv.BindRoles(bindings)
	require.ErrorContains(t, err, "reviewed exception")
	inv.Sites[1].Exception = "verified by a separate workload"
	work, err = inv.BindRoles(bindings)
	require.NoError(t, err)
	require.Len(t, work, 1)
	require.Len(t, work["reader"], 1)
}

func TestInventoryManualVariantAccess(t *testing.T) {
	access := []PolicyAccess{{Table: "public.notes", Command: "SELECT"}}
	for _, site := range []SQLSite{
		{ID: "query", Kind: "composed_query"}, {ID: "aggregate", Kind: "aggregate"},
		{ID: "builder", Dynamic: true, Exception: "reviewed alternatives"},
	} {
		t.Run(site.ID, func(t *testing.T) {
			b := SQLBinding{Roles: []string{"reader"}, Variants: []Statement{{Name: "first", SQL: "SELECT 1", Access: access}}, Access: access}
			inv := SQLInventory{Sites: []SQLSite{site}}
			bind := func() (RoleStatements, error) { return inv.BindRoles(map[string]SQLBinding{site.ID: b}) }
			work, err := bind()
			require.Nil(t, work)
			require.ErrorContains(t, err, "Access on each variant")
			b.Access = nil
			work, err = bind()
			require.NoError(t, err)
			require.Equal(t, []Statement{{Name: site.ID + "/first", SQL: "SELECT 1", Access: access}}, work["reader"])
			require.Equal(t, "first", b.Variants[0].Name)
			b.Variants[0].Name = " "
			_, err = bind()
			require.ErrorContains(t, err, "unnamed variant")
		})
	}
}

func TestRegisteredScalarAggregatesFromSnapshot(t *testing.T) {
	type row struct {
		ID    int64  `db:"id,key"`
		Scope string `db:"scope,scope"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[string]{})
	tabula.Define(reg, tabula.TableSpec[row, int64]{Name: "records", Aggregates: tabula.ScalarAggregates{
		Max: []string{"id"}, Min: []string{"id"}, Sum: []string{"id"}, Avg: []string{"id"},
	}})
	defs, err := ParseSnapshot(RenderSnapshot(reg.Defined()))
	require.NoError(t, err)
	composed := SQLInventory{Sites: []SQLSite{{ID: "view", Kind: "composed_query", Definition: "records", QueryKey: "max:id"}}}
	_, err = composed.BindRolesWithDefinitions(map[string]SQLBinding{"view": {Roles: []string{"reader"}}}, defs, nil)
	require.ErrorContains(t, err, "no registered variants")
	for _, op := range []string{"max", "min", "sum", "avg"} {
		site := SQLSite{ID: "call", Kind: "aggregate", Definition: "records", QueryKey: op + ":id", NoFilter: true}
		inv := SQLInventory{Sites: []SQLSite{site}}
		bindings := map[string]SQLBinding{"call": {Roles: []string{"reader"}}}
		work, err := inv.BindRolesWithDefinitions(bindings, defs, nil)
		require.NoError(t, err)
		require.Len(t, work["reader"], 2)
		require.Equal(t, "call/records/"+op+"_id", work["reader"][0].Name)
		require.Contains(t, work["reader"][0].SQL, "scope = $1")
		require.Equal(t, "call/records/unscoped_"+op+"_id", work["reader"][1].Name)
		require.NotContains(t, work["reader"][1].SQL, "scope")
		inv.Sites[0].NoFilter = false
		_, err = inv.BindRolesWithDefinitions(bindings, defs, nil)
		require.ErrorContains(t, err, "filter callback")
		inv.Sites[0].NoFilter = true
		// A manually named statement, including one from an older snapshot,
		// cannot stand in for the owned aggregate.
		delete(defs[0].StatementQueries, op+"_id")
		_, err = inv.BindRolesWithDefinitions(bindings, defs, nil)
		require.ErrorContains(t, err, "named Variants")
	}
}
