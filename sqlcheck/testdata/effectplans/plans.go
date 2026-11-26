// Package effectplans contains source declarations for the inventory tests.
package effectplans

import (
	"context"

	tb "github.com/0xJean/tabula"
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
var rel = tb.FromTable(table, "i", tb.Global())
var id = tb.FieldOf(rel, "id", tb.Int8())
var key = tb.Input("id", tb.Int8())

var rows = tb.DefineRows(entries, "select_items", tb.RowsFrom(table, rel),
	tb.RowsSpec{Lock: tb.ForRows(tb.ForUpdate, rel, tb.SkipLocked)},
	tb.Project(tb.Output(entries, "id", tb.Int8()), id.Expr()))
var lock, _ = tb.TryDefineAdvisoryLock(notes, "hold", tb.AdvisoryXactLock(tb.Exclusive, tb.Try, key.Expr()))

var scalar = tb.DeclareRoutine(tb.RoutineSpec[int64]{
	Schema: "public", Name: "item_count", Result: tb.Int8(), Cardinality: tb.RoutineOne, ReadOnly: true,
})
var set = tb.DeclareRoutine(tb.RoutineSpec[int64]{
	Schema: "public", Name: "update_items", Result: tb.Int8(), Cardinality: tb.RoutineSet,
})
var count = tb.DefineRoutine(entries, "count_items", scalar)
var update, _ = tb.TryDefineRoutine(entries, "change_items", set)

// Definitions exposes the same plan snapshots used by source inventory.
func Definitions() ([]tb.Definition, []tb.ViewDefinition) { return reg.Defined(), reg.DefinedViews() }

func use(ctx context.Context, s tb.Session[struct{}], indirect *tb.BoundRoutinePlan[result, struct{}]) {
	_, _ = rows.Bind(s).Find(ctx, tb.RowChoice{})
	_, _ = (*tb.BoundAdvisoryLockPlan[flag, struct{}]).Acquire(lock.Bind(s), ctx, tb.BindValue(key, 1))
	_, _ = count.Bind(s).One(ctx)
	_, _ = update.Bind(s).All(ctx)
	_ = indirect.All
	_ = count.Bind(s).One
}

func arguments() (*tb.View[flag, struct{}], string, tb.AdvisoryLock) {
	return notes, "hold", tb.AdvisoryXactLock(tb.Exclusive, tb.Try, key.Expr())
}

func fromTuple() {
	_, _ = tb.TryDefineAdvisoryLock(arguments())
}
