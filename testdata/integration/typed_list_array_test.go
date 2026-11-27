//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	tplan "github.com/0xJean/tabula/plan"
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
			g := tplan.FromTable(groups, "g", q.ByScopeColumn())
			scope := tplan.FieldOf(g, "scope_id", q.CatalogType[itemScope]("pg_catalog", "int8", nil))
			source := tplan.RowsFrom(groups, g, scope.Column())
			id := tplan.FieldOf(g, "id", tplan.Int8())
			ids, label := tplan.Input("ids", q.ArrayOf(tplan.Int8())), tplan.Input("label", tplan.Text())
			out := q.DefineView(reg, q.ViewSpec[planResult]{Name: "entries"})
			plan := tplan.DefineRows(out, "page", source, tplan.RowsSpec{
				RowFilter: tplan.RowFilter{Where: tplan.Any(id.Expr(), ids.Expr())},
				Orders:    []tplan.RowOrder{{Name: "id", Terms: []tplan.RowTerm{tplan.Asc(id)}}},
				Pages:     tplan.RowPages{Offset: true, MaxLimit: 5, MaxOffset: 5},
			}, tplan.Project(tplan.Output(out, "id", tplan.Int8()), id.Expr()),
				tplan.Project(tplan.Output(out, "label", tplan.Text()), label.Expr()))

			err := runRowPlan(t, db, pool, driver, reg.SQLAuthority(), role, 1, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				choice := tplan.RowChoice{Order: "id", Page: tplan.OffsetPage, Limit: 1, Offset: 1}
				bound := plan.Bind(s)
				page, total, err := bound.List(ctx, choice, tplan.BindValue(label, "visible"), tplan.BindValue(ids, []int64{1, 2, 4, 5}))
				require.NoError(t, err)
				require.EqualValues(t, 2, total)
				require.Equal(t, []planResult{{ID: 2, Label: new("visible")}}, page)
				for _, empty := range [][]int64{nil, {}} {
					page, total, err = bound.List(ctx, choice, tplan.BindValue(ids, empty), tplan.BindValue(label, "visible"))
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
