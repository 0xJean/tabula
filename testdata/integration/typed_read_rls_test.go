//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
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
			items := q.MapTable(q.DefinePlanSource(reg, q.TableSpec[listItem, int64]{Name: f.schema + ".items", RelationScope: q.ByRLS()})).As("i")
			notes := q.MapTable(q.DefinePlanSource(reg, q.TableSpec[listNote, int64]{Name: f.schema + ".notes", RelationScope: q.ByRLS()})).As("n")
			id, nid := q.FieldOf(items.Rel(), "id", q.Int8()), q.FieldOf(notes.Rel(), "id", q.Int8())
			out := q.DefineView(reg, q.ViewSpec[joinPair]{Name: "entries"})
			plan := q.DefineRead(out, "visible", q.Select(q.Project(q.Output(out, "group_id", q.Int8()), id.Expr()), q.Project(q.Output(out, "job_id", q.Int8()), nid.Expr())).From(items).LeftJoin(notes, q.Eq(q.FieldOf(notes.Rel(), "item_id", q.Int8()).Expr(), id.Expr())).
				WithList(q.ReadListSpec{Orders: []q.RowOrder{{Name: "id", Terms: []q.RowTerm{q.Asc(id), q.Asc(nid)}}}, Pages: q.RowPages{Keyset: true, MaxLimit: 10}, ExactCount: true}))
			def := out.Definition()
			require.Empty(t, def.StatementShapes["visible.rls.list.count"].Inputs)
			require.Len(t, def.StatementShapes["visible.rls.list.count"].ForceRLS, 2)
			require.NotContains(t, def.Statements["visible.rls.list.count"], "scope_id")
			f.reg = reg
			verifyListSnapshot(t, db, f)
			for _, scope := range []itemScope{1, 2} {
				err := runRowPlan(t, db, pool, driver, reg.SQLAuthority(), f.role, scope, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
					rows, total, err := plan.Bind(s).List(ctx, q.RowChoice{Order: "id", Page: q.FirstPage, Limit: 10})
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
