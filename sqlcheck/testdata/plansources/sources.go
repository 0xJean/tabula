package plansources

import (
	"context"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/plan"
)

type item struct {
	ID   int64 `db:"id,key"`
	Rank int32 `db:"rank"`
}

var reg = tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
var items, _ = tabula.TryDefinePlanSource(reg, tabula.TableSpec[item, int64]{Name: "public.items", RelationScope: tabula.ByRLS()})
var groups = tabula.DefinePlanSource(reg, tabula.TableSpec[item, int64]{Name: "public.groups", AllowDelete: true, RelationScope: tabula.Global()})
var i = plan.FromTable(items, "i", tabula.ByRLS())
var g = plan.FromTable(groups, "g", tabula.Global())
var id = plan.Input("id", plan.Int8())
var rank = plan.Input("rank", plan.Int4())
var change = plan.DefineUpdateMany(plan.RowsFrom(items, i), "change", plan.MutationSpec{RowFilter: filter(i)},
	plan.RowSet{Name: "rank", Assignments: []plan.RowAssignment{plan.Assign(plan.FieldOf(i, "rank", plan.Int4()), rank.Expr())}})
var remove = plan.DefineDeleteMany(plan.RowsFrom(groups, g), "remove", plan.MutationSpec{RowFilter: filter(g)})

func filter(r plan.Rel[item]) plan.RowFilter {
	return plan.RowFilter{Where: plan.Eq(plan.FieldOf(r, "id", plan.Int8()).Expr(), id.Expr())}
}

func use(ctx context.Context, s tabula.Session[struct{}]) {
	_, _ = change.Bind(s).UpdateMany(ctx, "rank", nil, plan.BindValue(id, 1), plan.BindValue(rank, 2))
	_, _ = remove.Bind(s).DeleteMany(ctx, nil, plan.BindValue(id, 1))
}

// Definitions supplies the physical sources for the inventory round trip.
func Definitions() []tabula.Definition { return reg.Defined() }
