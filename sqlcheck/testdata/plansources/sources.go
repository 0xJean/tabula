package plansources

import (
	"context"

	"github.com/0xJean/tabula"
)

type item struct {
	ID   int64 `db:"id,key"`
	Rank int32 `db:"rank"`
}

var reg = tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
var items, _ = tabula.TryDefinePlanSource(reg, tabula.TableSpec[item, int64]{Name: "public.items", RelationScope: tabula.ByRLS()})
var groups = tabula.DefinePlanSource(reg, tabula.TableSpec[item, int64]{Name: "public.groups", AllowDelete: true, RelationScope: tabula.Global()})
var i = tabula.FromTable(items, "i", tabula.ByRLS())
var g = tabula.FromTable(groups, "g", tabula.Global())
var id = tabula.Input("id", tabula.Int8())
var rank = tabula.Input("rank", tabula.Int4())
var change = tabula.DefineUpdateMany(tabula.RowsFrom(items, i), "change", tabula.MutationSpec{RowFilter: filter(i)},
	tabula.RowSet{Name: "rank", Assignments: []tabula.RowAssignment{tabula.Assign(tabula.FieldOf(i, "rank", tabula.Int4()), rank.Expr())}})
var remove = tabula.DefineDeleteMany(tabula.RowsFrom(groups, g), "remove", tabula.MutationSpec{RowFilter: filter(g)})

func filter(r tabula.Rel[item]) tabula.RowFilter {
	return tabula.RowFilter{Where: tabula.Eq(tabula.FieldOf(r, "id", tabula.Int8()).Expr(), id.Expr())}
}

func use(ctx context.Context, s tabula.Session[struct{}]) {
	_, _ = change.Bind(s).UpdateMany(ctx, "rank", nil, tabula.BindValue(id, 1), tabula.BindValue(rank, 2))
	_, _ = remove.Bind(s).DeleteMany(ctx, nil, tabula.BindValue(id, 1))
}

// Definitions supplies the physical sources for the inventory round trip.
func Definitions() []tabula.Definition { return reg.Defined() }
