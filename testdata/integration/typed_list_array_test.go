//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/stretchr/testify/require"
)

func TestListWithArrayFilter(t *testing.T) {
	db, pool := databases(t)
	schema, role := prepareJoinedTables(t, db)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			cfg := q.RegistryConfig[itemScope]{Deny: q.DenyLegacySQL}
			if driver == "stdsql" {
				cfg.ArrayArg = pqdriver.Array
			}
			reg := q.NewRegistry(cfg)
			groups := q.Define(reg, q.TableSpec[joinGroup, int64]{Name: schema + ".groups"})
			g := q.FromTable(groups, "g", q.ByScopeColumn())
			scope := q.FieldOf(g, "scope_id", q.CatalogType[itemScope]("pg_catalog", "int8", nil))
			source := q.RowsFrom(groups, g, scope.Column())
			id := q.FieldOf(g, "id", q.Int8())
			ids, label := q.Input("ids", q.ArrayOf(q.Int8())), q.Input("label", q.Text())
			out := q.DefineView(reg, q.ViewSpec[planResult]{Name: "entries"})
			plan := q.DefineRows(out, "page", source, q.RowsSpec{
				RowFilter: q.RowFilter{Where: q.Any(id.Expr(), ids.Expr())},
				Orders:    []q.RowOrder{{Name: "id", Terms: []q.RowTerm{q.Asc(id)}}},
				Pages:     q.RowPages{Offset: true, MaxLimit: 5, MaxOffset: 5},
			}, q.Project(q.Output(out, "id", q.Int8()), id.Expr()),
				q.Project(q.Output(out, "label", q.Text()), label.Expr()))

			err := runRowPlan(t, db, pool, driver, reg.SQLAuthority(), role, 1, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				choice := q.RowChoice{Order: "id", Page: q.OffsetPage, Limit: 1, Offset: 1}
				bound := plan.Bind(s)
				page, total, err := bound.List(ctx, choice, q.BindValue(label, "visible"), q.BindValue(ids, []int64{1, 2, 4, 5}))
				require.NoError(t, err)
				require.EqualValues(t, 2, total)
				require.Equal(t, []planResult{{ID: 2, Label: new("visible")}}, page)
				for _, empty := range [][]int64{nil, {}} {
					page, total, err = bound.List(ctx, choice, q.BindValue(ids, empty), q.BindValue(label, "visible"))
					require.NoError(t, err)
					require.Zero(t, total)
					require.Empty(t, page)
				}
				return nil
			})
			require.NoError(t, err)
		})
	}
}
