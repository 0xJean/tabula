// Package joinplans provides registered reads for inventory tests.
package joinplans

import (
	"context"

	q "github.com/0xJean/tabula"
)

type item struct {
	ID int64 `db:"id,key"`
}

type result struct {
	ID int64 `db:"id"`
}

var registry = q.NewRegistry(q.RegistryConfig[struct{}]{})
var items = q.Define(registry, q.TableSpec[item, int64]{Name: "public.items", RelationScope: q.ByRLS()})
var notes = q.Define(registry, q.TableSpec[item, int64]{Name: "public.notes", RelationScope: q.Global()})
var out = q.DefineView(registry, q.ViewSpec[result]{Name: "entries"})
var i, n = q.FromTable(items, "i", q.ByRLS()), q.FromTable(notes, "n", q.Global())
var id = q.FieldOf(i, "id", q.Int8())
var plan, _ = q.TryDefineRead(out, "match", q.Select(q.Project(q.Output(out, "id", q.Int8()), id.Expr())).
	From(i).InnerJoin(n, q.Eq(id.Expr(), q.FieldOf(n, "id", q.Int8()).Expr())))

// Definitions returns the registered outputs for role-bound verification.
func Definitions() []q.ViewDefinition { return registry.DefinedViews() }

func use(ctx context.Context, s q.Session[struct{}]) { _, _ = plan.Bind(s).Find(ctx) }
