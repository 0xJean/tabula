// Package inventoryfixture supplies real descriptors for the inventory CLI test.
package inventoryfixture

import (
	"context"

	"github.com/0xJean/tabula"
)

type projection struct {
	ID int64 `db:"id"`
}

type record struct {
	ID       int64  `db:"id,key"`
	Category string `db:"category"`
}

func query(source string) tabula.QuerySpec {
	return tabula.QuerySpec{
		Select: source, Scope: tabula.ScopeGlobal(), MaxLimit: 10,
		Order: tabula.QueryOrderSpec{Default: "id", Sorts: []tabula.QuerySort{{Key: "id", Expr: "id", Column: "id"}}},
	}
}

func Registry() *tabula.Registry[struct{}] {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	notes := tabula.DefineView(reg, tabula.ViewSpec[projection]{Name: "notes_view"})
	labels := tabula.DefineView(reg, tabula.ViewSpec[projection]{Name: "labels_view"})
	notes.DefineQuery("list", query("SELECT id FROM notes"))
	(*tabula.View[projection, struct{}]).DefineQuery(labels, "list", query("SELECT id FROM labels"))
	notes.DefineQuery("list.recent", query("SELECT id FROM recent_notes"))
	notes.Statement("list.extra", "SELECT id FROM archived_notes")
	tabula.Define(reg, tabula.TableSpec[record, int64]{Name: "records", ReadOnly: true, GroupBy: []string{"category"}})
	return reg
}

func counts(ctx context.Context, rows *tabula.Rows[record, int64, struct{}]) (map[string]int64, error) {
	return tabula.CountBy[string](ctx, rows, "category", nil)
}
