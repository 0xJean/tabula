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

func TestReadListRLSBothDrivers(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			f := newListFixture(t, db, driver)
			for _, table := range []string{"items", "notes"} {
				name := f.schema + "." + table
				for _, stmt := range []string{"ALTER TABLE " + name + " ENABLE ROW LEVEL SECURITY", "ALTER TABLE " + name + " FORCE ROW LEVEL SECURITY", "CREATE POLICY visible ON " + name + " FOR SELECT TO " + f.role + " USING (scope_id = nullif(current_setting('demo.scope',true),'')::bigint)", "REVOKE SELECT(scope_id) ON " + name + " FROM " + f.role} {
					_, err := db.ExecContext(t.Context(), stmt)
					require.NoError(t, err)
				}
			}
			cfg := q.RegistryConfig[itemScope]{Deny: q.DenyLegacySQL}
			if driver == "stdsql" {
				cfg.ArrayArg = pqdriver.Array
			}
			reg := q.NewRegistry(cfg)
			items := tplan.MapTable(q.DefinePlanSource(reg, q.TableSpec[listItem, int64]{Name: f.schema + ".items", RelationScope: q.ByRLS()})).As("i")
			notes := tplan.MapTable(q.DefinePlanSource(reg, q.TableSpec[listNote, int64]{Name: f.schema + ".notes", RelationScope: q.ByRLS()})).As("n")
			id, nid := tplan.FieldOf(items.Rel(), "id", tplan.Int8()), tplan.FieldOf(notes.Rel(), "id", tplan.Int8())
			out := q.DefineView(reg, q.ViewSpec[joinPair]{Name: "entries"})
			plan := tplan.DefineRead(out, "visible", tplan.Select(tplan.Project(tplan.Output(out, "group_id", tplan.Int8()), id.Expr()), tplan.Project(tplan.Output(out, "job_id", tplan.Int8()), nid.Expr())).From(items).LeftJoin(notes, tplan.Eq(tplan.FieldOf(notes.Rel(), "item_id", tplan.Int8()).Expr(), id.Expr())).
				WithList(tplan.ReadListSpec{Orders: []tplan.RowOrder{{Name: "id", Terms: []tplan.RowTerm{tplan.Asc(id), tplan.Asc(nid)}}}, Pages: tplan.RowPages{Keyset: true, MaxLimit: 10}, ExactCount: true}))
			def := out.Definition()
			require.Empty(t, def.StatementShapes["visible.rls.list.count"].Inputs)
			require.Len(t, def.StatementShapes["visible.rls.list.count"].ForceRLS, 2)
			require.NotContains(t, def.Statements["visible.rls.list.count"], "scope_id")
			f.reg = reg
			verifyListSnapshot(t, db, f)
			for _, scope := range []itemScope{1, 2} {
				err := runRowPlan(t, db, pool, driver, reg.SQLAuthority(), f.role, scope, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
					rows, total, err := plan.Bind(s).List(ctx, tplan.RowChoice{Order: "id", Page: tplan.FirstPage, Limit: 10})
					require.NoError(t, err)
					if scope == 1 {
						require.EqualValues(t, 6, total)
						require.Len(t, rows, 6)
					} else {
						require.EqualValues(t, 1, total)
						require.Equal(t, []joinPair{{Group: 5}}, rows)
					}
					return nil
				})
				require.NoError(t, err)
			}
		})
	}
}
