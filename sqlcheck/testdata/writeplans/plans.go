// Package writeplans exercises final write registrations and bound execution.
package writeplans

import (
	"context"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/plan"
)

type item struct {
	ID   int64 `db:"id,key"`
	Rank int32 `db:"rank"`
}

type result struct {
	ID int64 `db:"id"`
}

var registry = q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
var table = q.DefinePlanSource(registry, q.TableSpec[item, int64]{Name: "public.items", RelationScope: q.ByRLS(), AllowDelete: true})
var layout = plan.MapTable(table)
var source = layout.As("i")
var id = plan.MappedColumn[int64](layout, "id").On(source.Rel())
var rank = plan.MappedColumn[int32](layout, "rank").On(source.Rel())
var key = plan.Input("key", plan.Int8())
var value = plan.Input("value", plan.Int4())
var out = q.DefineView(registry, q.ViewSpec[result]{Name: "entries"})
var projection = plan.Project(plan.Output(out, "id", plan.Int8()), id.Expr())

var insert = plan.InsertInto(source).Values(plan.Assign(id, key.Expr()), plan.Assign(rank, value.Expr()))
var count, _ = plan.TryDefineWrite(source, "insert_count", insert)
var batch = plan.DefineWrite(source, "insert_batch", insert.Batch(1, 3))
var returned = plan.DefineReturning(out, "insert_result", plan.WithReturning(insert, projection))
var returnedBatch, _ = plan.TryDefineReturning(out, "insert_results", plan.WithReturning(insert.Batch(1, 3), projection))
var mapped, _ = plan.TryDefineTableReturning(source, "mapped", insert, plan.SelectColumns(id.Column()))

var filter = plan.MutationSpec{RowFilter: plan.RowFilter{
	Where:    plan.Eq(id.Expr(), key.Expr()),
	Optional: []plan.RowPredicate{{Name: "positive", When: plan.Gt(rank.Expr(), value.Expr())}},
}}
var updated, _ = plan.TryDefineUpdateReturning(source, out, "change", filter,
	[]plan.RowSet{{Name: "rank", Assignments: []plan.RowAssignment{plan.Assign(rank, value.Expr())}}}, projection)
var deleted = plan.DefineDeleteReturning(source, out, "remove", filter, projection)

var notes = q.DefinePlanSource(registry, q.TableSpec[item, int64]{Name: "public.notes", RelationScope: q.ByRLS(), WriteMode: q.WriteInsertOnly})
var noteLayout = plan.MapTable(notes)
var noteSource = noteLayout.As("n")
var noteID = plan.MappedColumn[int64](noteLayout, "id").On(noteSource.Rel())
var noteRank = plan.MappedColumn[int32](noteLayout, "rank").On(noteSource.Rel())
var modified = plan.EffectCTE("modified", plan.InsertInto(noteSource).Values(plan.Assign(noteID, key.Expr()), plan.Assign(noteRank, value.Expr())))
var graph = plan.DefineReturning(out, "graph", plan.Select(projection).From(source).With(modified))

// Published is intentionally mutable outside this package; uses require a binding.
var Published = count

// Definitions exports the owners with all registered forms.
func Definitions() ([]q.Definition, []q.ViewDefinition) {
	return registry.Defined(), registry.DefinedViews()
}

func execute(ctx context.Context, session q.Session[struct{}], indirect *plan.BoundReturningPlan[result, struct{}]) {
	values := []plan.Binding{plan.BindValue(key, int64(1)), plan.BindValue(value, int32(2))}
	_, _ = count.Bind(session).Exec(ctx, values...)
	_, _ = batch.Bind(session).ExecBatch(ctx, [][]plan.Binding{values})
	bound := returned.Bind(session)
	_, _ = (*plan.BoundReturningPlan[result, struct{}]).All(bound, ctx, values...)
	stream := bound.Iter
	for range stream(ctx, values...) {
	}
	_, _ = returnedBatch.Bind(session).AllBatch(ctx, [][]plan.Binding{values})
	_, _ = mapped.Bind(session).All(ctx, values...)
	_, _ = updated.Bind(session).UpdateMany(ctx, "rank", []string{"positive"}, values...)
	for range updated.Bind(session).UpdateIter(ctx, "rank", nil, values...) {
	}
	_, _ = deleted.Bind(session).DeleteMany(ctx, []string{"positive"}, values...)
	for range deleted.Bind(session).DeleteIter(ctx, nil, values...) {
	}
	_, _ = graph.Bind(session).All(ctx, values...)
	_, _ = Published.Bind(session).Exec(ctx, values...)
	_, _ = indirect.All(ctx, values...)
}
