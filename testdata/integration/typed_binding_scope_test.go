//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/stretchr/testify/require"
)

func TestMappedPrivateInputsAcrossDrivers(t *testing.T) {
	type item struct {
		ID    int64     `db:"id,key"`
		Scope itemScope `db:"scope_id,scope"`
		State string    `db:"state,deleted"`
	}
	db, pool := databases(t)
	schema, role := prepareJoinedTables(t, db)
	table := schema + ".items"
	for _, sql := range []string{
		"CREATE TABLE " + table + "(id bigint PRIMARY KEY, scope_id bigint NOT NULL, state text NOT NULL)",
		"INSERT INTO " + table + " VALUES (1,1,'active'),(2,1,'active'),(3,1,'removed'),(4,2,'active')",
		"GRANT SELECT ON " + table + " TO " + role,
	} {
		_, err := db.ExecContext(t.Context(), sql)
		require.NoError(t, err)
	}
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			var idCalls, scopeCalls, liveCalls int
			rejected := errors.New("invalid input")
			idType := pg.Int8().WithEncoder(func(v int64) (any, error) {
				idCalls++
				if v < 0 {
					return nil, rejected
				}
				return v + 1, nil
			})
			scopeType := q.CatalogType[itemScope]("pg_catalog", "int8", nil).WithEncoder(func(s itemScope) (any, error) {
				scopeCalls++
				if s < 0 {
					return nil, rejected
				}
				return int64(s + 1), nil
			})
			liveType := pg.Text().WithEncoder(func(v string) (any, error) {
				liveCalls++
				if v != "open" {
					return nil, rejected
				}
				return "active", nil
			})
			reg := q.NewRegistry(q.RegistryConfig[itemScope]{Deny: q.DenyLegacySQL})
			items := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: table,
				SoftDelete: &q.SoftDelete{DeletedValue: "closed", AliveValue: "open"}})
			layout := tplan.MapTable(items, tplan.ColumnType("id", idType), tplan.ColumnType("scope_id", scopeType), tplan.ColumnType("state", liveType))
			src := layout.As("i")
			id := tplan.MappedColumn[int64](layout, "id")
			field, floor := id.On(src.Rel()), id.Input("floor")
			plan := tplan.DefineTableRows(src, "by_id", tplan.RowsSpec{
				RowFilter: tplan.RowFilter{Where: tplan.Ge(field.Expr(), floor.Expr())},
				Orders:    []tplan.RowOrder{{Name: "forward", Terms: []tplan.RowTerm{tplan.Asc(field)}}},
				Pages:     tplan.RowPages{Keyset: true, MaxLimit: 8},
			}, tplan.SelectColumns(field.Column()))
			binding := tplan.BindValue(floor, 0)
			cursor, err := plan.Cursor("forward", tplan.At(field, int64(0)))
			require.NoError(t, err)
			require.Equal(t, 2, idCalls)
			require.Equal(t, 1, liveCalls)
			_, err = plan.Cursor("forward", tplan.At(field, int64(-1)))
			require.ErrorIs(t, err, rejected)
			choice := tplan.RowChoice{Order: "forward", Page: tplan.AfterPage, Limit: 8, After: cursor}
			require.NoError(t, runRowPlan(t, db, pool, driver, reg.SQLAuthority(), role, 0, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				for range 2 {
					rows, total, err := plan.Bind(s).List(ctx, choice, binding)
					require.NoError(t, err)
					require.Equal(t, []item{{ID: 2}}, rows)
					require.EqualValues(t, 2, total)
				}
				return nil
			}))
			require.Equal(t, 3, idCalls)
			require.Equal(t, 4, scopeCalls)
			require.Equal(t, 1, liveCalls)
			err = runRowPlan(t, db, pool, driver, reg.SQLAuthority(), role, -1, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				_, err := plan.Bind(s).Find(ctx, choice, binding)
				return err
			})
			require.ErrorIs(t, err, rejected)
		})
	}
}
