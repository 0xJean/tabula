// Package mappedreads provides mapped single-row plans for inventory tests.
package mappedreads

import (
	"context"

	q "github.com/0xJean/tabula"
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
var layout, _ = q.TryMapTable(table)
var src = layout.As("i")
var id = q.MappedColumn[int64](layout, "id").On(src.Rel())
var key = q.Input("key", q.Int8())
var filter = q.RowFilter{Optional: []q.RowPredicate{{Name: "first", When: q.Eq(id.Expr(), key.Expr())}}}
var spec = q.RowsSpec{
	RowFilter: filter,
	Orders:    []q.RowOrder{{Name: "offset", Terms: []q.RowTerm{q.Asc(id)}}},
	Pages:     q.RowPages{Unpaged: true, Offset: true, MaxLimit: 3},
}
var lookup, _ = q.TryDefineTableRows(src, "lookup", spec, q.SelectColumns(id.Column()), q.ReadForms{Exact: true, First: q.OrderedFirst})
var visible = q.DefineExists(q.MapTable(table).As("visible"), "visible", q.RowFilter{})
var out = q.DefineView(registry, q.ViewSpec[result]{Name: "entries"})
var projection = q.Project(q.Output(out, "id", q.Int8()), id.Expr())
var unlocked = q.DefineRowsWithForms(out, "unlocked", src, spec, q.ReadForms{First: q.OrderedFirst}, projection)
var peer = layout.As("peer")
var brief, _ = q.TryDefineTableRead(src, "brief", q.SelectColumns(id.Column()), q.Select())
var peerID = q.MappedColumn[int64](layout, "id").On(peer.Rel())
var joined, _ = q.TryDefineRead(out, "joined", q.Select(projection).From(src).InnerJoin(peer, q.Eq(peerID.Expr(), id.Expr())).
	WithForms(q.ReadForms{Exact: true}))

// Definitions returns both owning descriptors for snapshot verification.
func Definitions() ([]q.Definition, []q.ViewDefinition) {
	return registry.Defined(), registry.DefinedViews()
}

func use(ctx context.Context, s q.Session[struct{}]) {
	bound := lookup.Bind(s)
	_, _ = bound.One(ctx, q.RowChoice{Order: "offset"})
	_, _, _ = bound.MaybeFirst(ctx, q.RowChoice{Order: "offset"})
	_, _ = visible.Bind(s).Exists(ctx, nil)
	_, _, _ = unlocked.Bind(s).MaybeOne(ctx, q.RowChoice{Order: "offset"})
	_, _ = unlocked.Bind(s).First(ctx, q.RowChoice{Order: "offset"})
	_, _ = joined.Bind(s).One(ctx)
	_, _ = brief.Bind(s).Find(ctx)
}
