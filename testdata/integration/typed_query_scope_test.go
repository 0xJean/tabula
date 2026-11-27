//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
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
			scoped, rls := q.MapTable(table).As("i"), q.MapTable(policy).As("i")
			wanted := q.Input("wanted", q.Int8())
			first := q.Export("value", q.CountAll())
			common := q.CTE("groups", q.Select(first.Project()).From(scoped).Where(q.Ge(q.FieldOf(scoped.Rel(), "id", q.Int8()).Expr(), wanted.Expr())), q.Materialized)
			c := common.As("c")
			out := q.DefineView(reg, q.ViewSpec[counts]{Name: "entries"})
			plan := q.DefineRead(out, "counts", q.Select(
				q.Project(q.Output(out, "scoped", q.Int8()), q.ScalarSubquery(q.SelectValue(first.From(c)).From(c))),
				q.Project(q.Output(out, "policy", q.Int8()), q.ScalarSubquery(q.SelectValue(q.CountAll()).From(rls).Where(q.Ge(q.FieldOf(rls.Rel(), "id", q.Int8()).Expr(), wanted.Expr())))),
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
					rows, err := plan.Bind(s).Find(ctx, q.BindValue(wanted, int64(1)))
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
				_, err := plan.Bind(s).Find(ctx, q.BindValue(wanted, int64(1)))
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
			source := q.MapTable(table).As("i")
			value := q.Export("value", q.FieldOf(source.Rel(), "id", q.Int8()).Expr())
			d := q.Derived("d", q.Select(value.Project()).From(source).Lock(q.ForRows(q.ForShare, source.Rel(), q.Wait)))
			out := q.DefineView(reg, q.ViewSpec[composedValue]{Name: "entries"})
			plan := q.DefineRead(out, "locked", q.Select(q.Project(q.Output(out, "value", q.Int8()), value.From(d))).From(d))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			err = run(t.Context(), func(ctx context.Context, s session) error { _, err := plan.Bind(s).Find(ctx); return err })
			require.ErrorIs(t, err, q.ErrReadOnly)
			direct := effectDirect(t, driver, reg.SQLAuthority())
			_, err = plan.Bind(direct).Find(t.Context())
			require.ErrorIs(t, err, q.ErrReadOnly)
		})
	}
}
