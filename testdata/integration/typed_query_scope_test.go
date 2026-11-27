//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

func TestQueryScopeAndPrivilegesDrivers(t *testing.T) {
	type item struct {
		ID    int64     `db:"id,key"`
		Scope itemScope `db:"scope_id,scope"`
	}
	type counts struct {
		Scoped *int64 `db:"scoped"`
		Policy *int64 `db:"policy"`
	}
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			schema := typesSchema(t, db)
			role := schema + "_reader"
			exec := func(sql string) { t.Helper(); _, err := db.ExecContext(t.Context(), sql); require.NoError(t, err) }
			exec("CREATE ROLE " + role + " NOBYPASSRLS")
			t.Cleanup(func() {
				_, err := db.ExecContext(context.Background(), "DROP OWNED BY "+role+"; DROP ROLE "+role)
				require.NoError(t, err)
			})
			exec("CREATE TABLE " + schema + ".items(id bigint PRIMARY KEY,scope_id bigint NOT NULL); CREATE TABLE " + schema + ".notes (LIKE " + schema + ".items INCLUDING ALL)")
			exec("INSERT INTO " + schema + ".items VALUES (1,1),(2,1),(3,2); INSERT INTO " + schema + ".notes SELECT * FROM " + schema + ".items")
			exec("ALTER TABLE " + schema + ".notes ENABLE ROW LEVEL SECURITY; ALTER TABLE " + schema + ".notes FORCE ROW LEVEL SECURITY")
			exec("CREATE POLICY scope_read ON " + schema + ".notes FOR SELECT USING (scope_id=NULLIF(current_setting('demo.scope',true),'')::bigint)")
			exec("GRANT USAGE ON SCHEMA " + schema + " TO " + role + "; GRANT SELECT(id,scope_id) ON " + schema + ".items," + schema + ".notes TO " + role)
			reg := q.NewRegistry(q.RegistryConfig[itemScope]{Deny: q.DenyLegacySQL, DefaultTypes: []q.TypeDefault{q.TypeMapping(q.CatalogType[itemScope]("pg_catalog", "int8", nil))}})
			table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: schema + ".items", RelationScope: q.ByScopeColumn()})
			policy := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: schema + ".notes", RelationScope: q.ByRLS()})
			scoped, rls := tplan.MapTable(table).As("i"), tplan.MapTable(policy).As("i")
			wanted := tplan.Input("wanted", tplan.Int8())
			first := tplan.Export("value", tplan.CountAll())
			common := tplan.CTE("groups", tplan.Select(first.Project()).From(scoped).Where(tplan.Ge(tplan.FieldOf(scoped.Rel(), "id", tplan.Int8()).Expr(), wanted.Expr())), tplan.Materialized)
			c := common.As("c")
			out := q.DefineView(reg, q.ViewSpec[counts]{Name: "entries"})
			plan := tplan.DefineRead(out, "counts", tplan.Select(
				tplan.Project(tplan.Output(out, "scoped", tplan.Int8()), tplan.ScalarSubquery(tplan.SelectValue(first.From(c)).From(c))),
				tplan.Project(tplan.Output(out, "policy", tplan.Int8()), tplan.ScalarSubquery(tplan.SelectValue(tplan.CountAll()).From(rls).Where(tplan.Ge(tplan.FieldOf(rls.Rel(), "id", tplan.Int8()).Expr(), wanted.Expr())))),
			).With(common))
			shape := out.Definition().StatementShapes["counts.scoped.data"]
			require.Len(t, shape.Inputs, 2)
			require.True(t, shape.Inputs[0].Scope)
			require.Equal(t, []string{`"` + schema + `"."notes"`}, shape.ForceRLS)
			work := sqlcheck.RoleStatements{}
			work.AddDefinitions(role, sqlcheck.ViewsToDefinitions(reg.DefinedViews())...)
			findings, err := sqlcheck.VerifyRoleStatements(t.Context(), db, work)
			require.NoError(t, err)
			require.Empty(t, findings)
			for _, scope := range []itemScope{1, 2} {
				require.NoError(t, runRowPlan(t, db, pool, driver, reg.SQLAuthority(), role, scope, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
					rows, err := plan.Bind(s).Find(ctx, tplan.BindValue(wanted, int64(1)))
					require.NoError(t, err)
					require.Len(t, rows, 1)
					want := int64(2)
					if scope == 2 {
						want = 1
					}
					require.Equal(t, counts{new(want), new(want)}, rows[0])
					return nil
				}))
			}
			exec("REVOKE SELECT(id) ON " + schema + ".notes FROM " + role)
			err = runRowPlan(t, db, pool, driver, reg.SQLAuthority(), role, 1, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				_, err := plan.Bind(s).Find(ctx, tplan.BindValue(wanted, int64(1)))
				return err
			})
			require.Equal(t, "42501", q.SQLState(err))
			findings, err = sqlcheck.VerifyRoleStatements(t.Context(), db, work)
			require.NoError(t, err)
			require.NotEmpty(t, findings)
		})
	}
}

func TestQueryNestedLockRefusalDrivers(t *testing.T) {
	db, pool := databases(t)
	schema := typesSchema(t, db)
	_, err := db.ExecContext(t.Context(), "CREATE TABLE "+schema+".items(id bigint PRIMARY KEY,value bigint); INSERT INTO "+schema+".items VALUES (1,1)")
	require.NoError(t, err)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			table := q.DefinePlanSource(reg, q.TableSpec[composedItem, int64]{Name: schema + ".items", RelationScope: q.Global()})
			source := tplan.MapTable(table).As("i")
			value := tplan.Export("value", tplan.FieldOf(source.Rel(), "id", tplan.Int8()).Expr())
			d := tplan.Derived("d", tplan.Select(value.Project()).From(source).Lock(tplan.ForRows(tplan.ForShare, source.Rel(), tplan.Wait)))
			out := q.DefineView(reg, q.ViewSpec[composedValue]{Name: "entries"})
			plan := tplan.DefineRead(out, "locked", tplan.Select(tplan.Project(tplan.Output(out, "value", tplan.Int8()), value.From(d))).From(d))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			err = run(t.Context(), func(ctx context.Context, s session) error { _, err := plan.Bind(s).Find(ctx); return err })
			require.ErrorIs(t, err, q.ErrReadOnly)
			direct := effectDirect(t, driver, reg.SQLAuthority())
			_, err = plan.Bind(direct).Find(t.Context())
			require.ErrorIs(t, err, q.ErrReadOnly)
		})
	}
}
