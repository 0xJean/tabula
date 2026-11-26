// Package rowplans exercises source inventory for typed plan declarations and executions.
package rowplans

import (
	"context"

	tb "github.com/0xJean/tabula"
)

type item struct {
	ID   int64 `db:"id,key"`
	Rank int32 `db:"rank"`
}

type result struct {
	ID int64 `db:"id"`
}

var reg = tb.NewRegistry(tb.RegistryConfig[struct{}]{})
var table = tb.Define(reg, tb.TableSpec[item, int64]{Name: "public.items", RelationScope: tb.Global(), AllowDelete: true})
var out = tb.DefineView(reg, tb.ViewSpec[result]{Name: "entries"})
var rel = tb.FromTable(table, "i", tb.Global())
var src = tb.RowsFrom(table, rel)
var id = tb.FieldOf(rel, "id", tb.Int8())
var rank = tb.FieldOf(rel, "rank", tb.Int4())
var value = tb.Input("rank", tb.Int4())
var where = tb.RowFilter{Where: tb.Ge(rank.Expr(), value.Expr())}
var plan = tb.DefineRows(out, "items_by_rank", src, tb.RowsSpec{
	RowFilter: where, WithExists: true,
	Orders: []tb.RowOrder{{Name: "forward", Terms: []tb.RowTerm{tb.Asc(id)}}, {Name: "reverse", Terms: []tb.RowTerm{tb.Desc(id)}}},
}, tb.Project(tb.Output(out, "id", tb.Int8()), id.Expr()))
var change, _ = tb.TryDefineUpdateMany(src, "change_rank", tb.MutationSpec{RowFilter: where}, tb.RowSet{
	Name: "rank", Assignments: []tb.RowAssignment{tb.Assign(rank, value.Expr())},
})
var remove = tb.DefineDeleteMany(src, "remove_items", tb.MutationSpec{RowFilter: where})

// Definitions returns the matching snapshot inputs.
func Definitions() ([]tb.Definition, []tb.ViewDefinition) { return reg.Defined(), reg.DefinedViews() }

func use(ctx context.Context, s tb.Session[struct{}], indirect *tb.BoundRowsPlan[result, struct{}]) {
	bound := plan.Bind(s)
	_, _, _ = bound.List(ctx, tb.RowChoice{Order: "forward"}, tb.BindValue(value, 1))
	_, _ = (*tb.BoundRowsPlan[result, struct{}]).Count(bound, ctx, nil, tb.BindValue(value, 1))
	_, _ = bound.Exists(ctx, nil, tb.BindValue(value, 1))
	_, _ = indirect.Find(ctx, tb.RowChoice{}, tb.BindValue(value, 1))
	_, _ = change.Bind(s).UpdateMany(ctx, "rank", nil, tb.BindValue(value, 2))
	_, _ = remove.Bind(s).DeleteMany(ctx, nil, tb.BindValue(value, 3))
}
