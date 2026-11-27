// Package writeplans exercises final write registrations and bound execution.
package writeplans

import (
	"context"

	q "github.com/0xJean/tabula"
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
var layout = q.MapTable(table)
var source = layout.As("i")
var id = q.MappedColumn[int64](layout, "id").On(source.Rel())
var rank = q.MappedColumn[int32](layout, "rank").On(source.Rel())
var key = q.Input("key", q.Int8())
var value = q.Input("value", q.Int4())
var out = q.DefineView(registry, q.ViewSpec[result]{Name: "entries"})
var projection = q.Project(q.Output(out, "id", q.Int8()), id.Expr())

var insert = q.InsertInto(source).Values(q.Assign(id, key.Expr()), q.Assign(rank, value.Expr()))
var count, _ = q.TryDefineWrite(source, "insert_count", insert)
var batch = q.DefineWrite(source, "insert_batch", insert.Batch(1, 3))
var returned = q.DefineReturning(out, "insert_result", q.WithReturning(insert, projection))
var returnedBatch, _ = q.TryDefineReturning(out, "insert_results", q.WithReturning(insert.Batch(1, 3), projection))
var mapped, _ = q.TryDefineTableReturning(source, "mapped", insert, q.SelectColumns(id.Column()))

var filter = q.MutationSpec{RowFilter: q.RowFilter{
	Where:    q.Eq(id.Expr(), key.Expr()),
	Optional: []q.RowPredicate{{Name: "positive", When: q.Gt(rank.Expr(), value.Expr())}},
}}
var updated, _ = q.TryDefineUpdateReturning(source, out, "change", filter,
	[]q.RowSet{{Name: "rank", Assignments: []q.RowAssignment{q.Assign(rank, value.Expr())}}}, projection)
var deleted = q.DefineDeleteReturning(source, out, "remove", filter, projection)

var notes = q.DefinePlanSource(registry, q.TableSpec[item, int64]{Name: "public.notes", RelationScope: q.ByRLS(), WriteMode: q.WriteInsertOnly})
var noteLayout = q.MapTable(notes)
var noteSource = noteLayout.As("n")
var noteID = q.MappedColumn[int64](noteLayout, "id").On(noteSource.Rel())
var noteRank = q.MappedColumn[int32](noteLayout, "rank").On(noteSource.Rel())
var modified = q.EffectCTE("modified", q.InsertInto(noteSource).Values(q.Assign(noteID, key.Expr()), q.Assign(noteRank, value.Expr())))
var graph = q.DefineReturning(out, "graph", q.Select(projection).From(source).With(modified))

// Published is intentionally mutable outside this package; uses require a binding.
var Published = count

// Definitions exports the owners with all registered forms.
func Definitions() ([]q.Definition, []q.ViewDefinition) {
	return registry.Defined(), registry.DefinedViews()
}

func execute(ctx context.Context, session q.Session[struct{}], indirect *q.BoundReturningPlan[result, struct{}]) {
	values := []q.Binding{q.BindValue(key, int64(1)), q.BindValue(value, int32(2))}
	_, _ = count.Bind(session).Exec(ctx, values...)
	_, _ = batch.Bind(session).ExecBatch(ctx, [][]q.Binding{values})
	bound := returned.Bind(session)
	_, _ = (*q.BoundReturningPlan[result, struct{}]).All(bound, ctx, values...)
	stream := bound.Iter
	for range stream(ctx, values...) {
	}
	_, _ = returnedBatch.Bind(session).AllBatch(ctx, [][]q.Binding{values})
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
