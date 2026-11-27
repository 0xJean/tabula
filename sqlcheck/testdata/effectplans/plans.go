// Package effectplans contains source declarations for the inventory tests.
package effectplans

import (
	"context"

	tb "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/plan"
)

type item struct {
	ID int64 `db:"id,key"`
}

type result struct {
	ID int64 `db:"id"`
}

type flag struct {
	Held bool `db:"held"`
}

var reg = tb.NewRegistry(tb.RegistryConfig[struct{}]{})
var table = tb.Define(reg, tb.TableSpec[item, int64]{Name: "public.items", RelationScope: tb.Global()})
var entries = tb.DefineView(reg, tb.ViewSpec[result]{Name: "entries"})
var notes = tb.DefineView(reg, tb.ViewSpec[flag]{Name: "notes"})
var rel = plan.FromTable(table, "i", tb.Global())
var id = plan.FieldOf(rel, "id", plan.Int8())
var key = plan.Input("id", plan.Int8())

var rows = plan.DefineRows(entries, "select_items", plan.RowsFrom(table, rel),
	plan.RowsSpec{Lock: plan.ForRows(plan.ForUpdate, rel, plan.SkipLocked)},
	plan.Project(plan.Output(entries, "id", plan.Int8()), id.Expr()))
var lock, _ = plan.TryDefineAdvisoryLock(notes, "hold", plan.AdvisoryXactLock(plan.Exclusive, plan.Try, key.Expr()))

var scalar = plan.DeclareRoutine(plan.RoutineSpec[int64]{
	Schema: "public", Name: "item_count", Result: plan.Int8(), Cardinality: plan.RoutineOne, ReadOnly: true,
})
var set = plan.DeclareRoutine(plan.RoutineSpec[int64]{
	Schema: "public", Name: "update_items", Result: plan.Int8(), Cardinality: plan.RoutineSet,
})
var count = plan.DefineRoutine(entries, "count_items", scalar)
var update, _ = plan.TryDefineRoutine(entries, "change_items", set)

// Definitions exposes the same plan snapshots used by source inventory.
func Definitions() ([]tb.Definition, []tb.ViewDefinition) { return reg.Defined(), reg.DefinedViews() }

func use(ctx context.Context, s tb.Session[struct{}], indirect *plan.BoundRoutinePlan[result, struct{}]) {
	_, _ = rows.Bind(s).Find(ctx, plan.RowChoice{})
	_, _ = (*plan.BoundAdvisoryLockPlan[flag, struct{}]).Acquire(lock.Bind(s), ctx, plan.BindValue(key, 1))
	_, _ = count.Bind(s).One(ctx)
	_, _ = update.Bind(s).All(ctx)
	_ = indirect.All
	_ = count.Bind(s).One
}

func arguments() (*tb.View[flag, struct{}], string, plan.AdvisoryLock) {
	return notes, "hold", plan.AdvisoryXactLock(plan.Exclusive, plan.Try, key.Expr())
}

func fromTuple() {
	_, _ = plan.TryDefineAdvisoryLock(arguments())
}
