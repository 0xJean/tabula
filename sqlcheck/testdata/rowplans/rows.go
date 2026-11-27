// Package rowplans exercises source inventory for typed plan declarations and executions.
package rowplans

import (
	"context"

	tb "github.com/0xJean/tabula"
	tplan "github.com/0xJean/tabula/plan"
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
var rel = tplan.FromTable(table, "i", tb.Global())
var src = tplan.RowsFrom(table, rel)
var id = tplan.FieldOf(rel, "id", tplan.Int8())
var rank = tplan.FieldOf(rel, "rank", tplan.Int4())
var value = tplan.Input("rank", tplan.Int4())
var where = tplan.RowFilter{Where: tplan.Ge(rank.Expr(), value.Expr())}
var plan = tplan.DefineRows(out, "items_by_rank", src, tplan.RowsSpec{
	RowFilter: where, WithExists: true,
	Orders: []tplan.RowOrder{{Name: "forward", Terms: []tplan.RowTerm{tplan.Asc(id)}}, {Name: "reverse", Terms: []tplan.RowTerm{tplan.Desc(id)}}},
}, tplan.Project(tplan.Output(out, "id", tplan.Int8()), id.Expr()))
var change, _ = tplan.TryDefineUpdateMany(src, "change_rank", tplan.MutationSpec{RowFilter: where}, tplan.RowSet{
	Name: "rank", Assignments: []tplan.RowAssignment{tplan.Assign(rank, value.Expr())},
})
var remove = tplan.DefineDeleteMany(src, "remove_items", tplan.MutationSpec{RowFilter: where})

// Definitions returns the matching snapshot inputs.
func Definitions() ([]tb.Definition, []tb.ViewDefinition) { return reg.Defined(), reg.DefinedViews() }

func use(ctx context.Context, s tb.Session[struct{}], indirect *tplan.BoundRowsPlan[result, struct{}]) {
	bound := plan.Bind(s)
	_, _, _ = bound.List(ctx, tplan.RowChoice{Order: "forward"}, tplan.BindValue(value, 1))
	_, _ = (*tplan.BoundRowsPlan[result, struct{}]).Count(bound, ctx, nil, tplan.BindValue(value, 1))
	_, _ = bound.Exists(ctx, nil, tplan.BindValue(value, 1))
	_, _ = indirect.Find(ctx, tplan.RowChoice{}, tplan.BindValue(value, 1))
	_, _ = change.Bind(s).UpdateMany(ctx, "rank", nil, tplan.BindValue(value, 2))
	_, _ = remove.Bind(s).DeleteMany(ctx, nil, tplan.BindValue(value, 3))
}
