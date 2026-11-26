package sqlcheck

import (
	"testing"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestExplicitReadInventorySeparatesPagesAndCounts(t *testing.T) {
	const plan = "report"
	const prefix = "report.global."
	const owner = "public.items"
	data, total := prefix+"list.data.sort.where.after.where.active", prefix+"list.count.where.active"
	d := tabula.Definition{
		Name: owner, Statements: map[string]string{}, StatementQueries: map[string]string{},
		StatementShapes: map[string]tabula.StatementShape{},
	}
	for _, key := range []string{prefix + "data", prefix + "single", prefix + "list.count", data, total} {
		d.Statements[key] = "SELECT id FROM public.items"
		d.StatementQueries[key] = plan
		d.StatementShapes[key] = tabula.StatementShape{
			Effects: tabula.EffectRead, Access: []tabula.ShapeAccess{{Relation: owner, Command: "SELECT"}},
		}
	}
	definitions := map[string][]tabula.Definition{owner: {d}}
	site := SQLSite{ID: "site", Kind: "typed_rows", Definition: owner, QueryKey: plan, Receiver: tabulaPkgPath + ".BoundReadPlan"}
	check := func(method, binding string, want ...string) {
		t.Helper()
		site.Method = method
		stmts, err := site.bindRowPlan(SQLBinding{Prefix: binding}, definitions)
		require.NoError(t, err)
		var names []string
		for _, stmt := range stmts {
			names = append(names, stmt.Name)
		}
		var qualified []string
		for _, key := range want {
			qualified = append(qualified, "site/"+owner+"/"+key)
		}
		require.ElementsMatch(t, qualified, names)
	}
	check("Find", "", prefix+"data")
	check("One", "", prefix+"single")
	check("Count", "", prefix+"list.count", total)
	check("Page", "", data)
	check("List", data, data, total)
	delete(d.StatementShapes, total)
	_, err := site.bindRowPlan(SQLBinding{Prefix: data}, definitions)
	require.ErrorContains(t, err, "missing typed statement metadata")
	delete(d.StatementQueries, total)
	_, err = site.bindRowPlan(SQLBinding{Prefix: data}, definitions)
	require.ErrorContains(t, err, "missing count shape")
}
