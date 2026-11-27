// Package mappedreads provides mapped single-row plans for inventory tests.
package mappedreads

import (
	"context"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/plan"
)

type item struct {
	ID   int64   `db:"id,key"`
	Note *string `db:"note"`
}

type result struct {
	ID int64 `db:"id"`
}

var registry = q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
var table, _ = q.TryDefinePlanSource(registry, q.TableSpec[item, int64]{Name: "public.items", RelationScope: q.ByRLS()})
var layout, _ = plan.TryMapTable(table)
var src = layout.As("i")
var id = plan.MappedColumn[int64](layout, "id").On(src.Rel())
var key = plan.Input("key", plan.Int8())
var filter = plan.RowFilter{Optional: []plan.RowPredicate{{Name: "first", When: plan.Eq(id.Expr(), key.Expr())}}}
var spec = plan.RowsSpec{
	RowFilter: filter,
	Orders:    []plan.RowOrder{{Name: "offset", Terms: []plan.RowTerm{plan.Asc(id)}}},
	Pages:     plan.RowPages{Unpaged: true, Offset: true, MaxLimit: 3},
}
var lookup, _ = plan.TryDefineTableRows(src, "lookup", spec, plan.SelectColumns(id.Column()), plan.ReadForms{Exact: true, First: plan.OrderedFirst})
var visible = plan.DefineExists(plan.MapTable(table).As("visible"), "visible", plan.RowFilter{})
var out = q.DefineView(registry, q.ViewSpec[result]{Name: "entries"})
var projection = plan.Project(plan.Output(out, "id", plan.Int8()), id.Expr())
var unlocked = plan.DefineRowsWithForms(out, "unlocked", src, spec, plan.ReadForms{First: plan.OrderedFirst}, projection)
var peer = layout.As("peer")
var caption = plan.MappedColumn[string](layout, "note").On(src.Rel())
var brief, _ = plan.TryDefineTableRead(src, "brief", plan.SelectColumns(caption.Column()), plan.Select().WithList(plan.ReadListSpec{
	Optional: filter.Optional, Orders: spec.Orders,
	Pages: plan.RowPages{Offset: true, MaxLimit: 3, MaxOffset: 6}, ExactCount: true,
}))
var peerID = plan.MappedColumn[int64](layout, "id").On(peer.Rel())
var joined, _ = plan.TryDefineRead(out, "joined", plan.Select(projection).From(src).InnerJoin(peer, plan.Eq(peerID.Expr(), id.Expr())).
	WithForms(plan.ReadForms{Exact: true}).WithList(plan.ReadListSpec{
	Orders: spec.Orders, Pages: plan.RowPages{Offset: true, MaxLimit: 3, MaxOffset: 6}, ExactCount: true,
}))

// Definitions returns both owning descriptors for snapshot verification.
func Definitions() ([]q.Definition, []q.ViewDefinition) {
	return registry.Defined(), registry.DefinedViews()
}

func use(ctx context.Context, s q.Session[struct{}]) {
	bound := lookup.Bind(s)
	_, _ = bound.One(ctx, plan.RowChoice{Order: "offset"})
	_, _, _ = bound.MaybeFirst(ctx, plan.RowChoice{Order: "offset"})
	_, _ = visible.Bind(s).Exists(ctx, nil)
	_, _, _ = unlocked.Bind(s).MaybeOne(ctx, plan.RowChoice{Order: "offset"})
	_, _ = unlocked.Bind(s).First(ctx, plan.RowChoice{Order: "offset"})
	_, _ = joined.Bind(s).One(ctx)
	page := plan.RowChoice{Page: plan.OffsetPage, Limit: 1, Order: "offset"}
	_, _ = brief.Bind(s).Find(ctx)
	_, _ = brief.Bind(s).Count(ctx, nil)
	_, _ = brief.Bind(s).Page(ctx, page)
	_, _, _ = brief.Bind(s).List(ctx, page)
	_, _ = joined.Bind(s).Count(ctx, nil)
	_, _ = joined.Bind(s).Page(ctx, page)
	_, _, _ = joined.Bind(s).List(ctx, page)
}
