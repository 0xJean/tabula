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

func TestQueryCatalogLineageDrivers(t *testing.T) {
	type item struct {
		ID    int64   `db:"id,key"`
		Value *string `db:"value"`
	}
	type result struct {
		Value *string `db:"value"`
	}
	db, pool := databases(t)
	schema := typesSchema(t, db)
	_, err := db.ExecContext(t.Context(), "CREATE TYPE "+schema+".label_kind AS ENUM ('a','b'); CREATE DOMAIN "+schema+".label_value AS text; CREATE TABLE "+schema+".items(id bigint PRIMARY KEY,value "+schema+".label_kind); CREATE TABLE "+schema+".notes(id bigint PRIMARY KEY,value "+schema+".label_value); INSERT INTO "+schema+".items VALUES(1,'a'),(2,'b'),(3,NULL); INSERT INTO "+schema+".notes VALUES(1,'a'),(2,'b'),(3,NULL)")
	require.NoError(t, err)
	var role string
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT current_user").Scan(&role))
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			for _, test := range []struct{ table, typ string }{{"items", "label_kind"}, {"notes", "label_value"}} {
				t.Run(test.typ, func(t *testing.T) {
					reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
					kind := q.CatalogType[string](schema, test.typ, nil)
					table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: schema + "." + test.table, RelationScope: q.Global()})
					source := tplan.MapTable(table, tplan.ColumnType("value", kind)).As("i")
					field := tplan.FieldOf(source.Rel(), "value", kind)
					input := tplan.Input("wanted", kind)
					value := tplan.Export("value", field.Expr())
					common := tplan.CTE("groups", tplan.Select(value.Project()).From(source).Where(tplan.Eq(field.Expr(), input.Expr())), tplan.Materialized)
					n := common.As("n")
					out := q.DefineView(reg, q.ViewSpec[result]{Name: "entries"})
					output := tplan.Output(out, "value", kind)
					selected := tplan.Select(tplan.Project(output, value.From(n))).From(n).With(common)
					d := tplan.Derived("d", selected)
					plan := tplan.DefineRead(out, "typed", tplan.Select(tplan.Project(output, tplan.ResultOf(d, output))).From(d))
					run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
					require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
						rows, err := plan.Bind(s).Find(ctx, tplan.BindValue(input, "a"))
						require.NoError(t, err)
						require.Equal(t, []result{{new("a")}}, rows)
						return nil
					}))
					def := out.Definition()
					shape := def.StatementShapes["typed.global.data"]
					require.Equal(t, `"`+schema+`"."`+test.typ+`"`, shape.Result[0].Type)
					tx, err := db.BeginTx(t.Context(), nil)
					require.NoError(t, err)
					_, err = tx.ExecContext(t.Context(), "PREPARE query_catalog AS "+def.Statements["typed.global.data"])
					require.NoError(t, err)
					var actual, expected uint32
					require.NoError(t, tx.QueryRowContext(t.Context(), "SELECT parameter_types[1]::oid FROM pg_prepared_statements WHERE name='query_catalog'").Scan(&actual))
					require.NoError(t, tx.QueryRowContext(t.Context(), "SELECT oid FROM pg_type WHERE typnamespace=$1::regnamespace AND typname=$2", schema, test.typ).Scan(&expected))
					require.Equal(t, expected, actual)
					_, err = tx.ExecContext(t.Context(), "DEALLOCATE query_catalog")
					require.NoError(t, err)
					require.NoError(t, tx.Rollback())
					work := sqlcheck.RoleStatements{}
					work.AddDefinitions(role, sqlcheck.ViewsToDefinitions(reg.DefinedViews())...)
					findings, err := sqlcheck.VerifyRoleStatements(t.Context(), db, work)
					require.NoError(t, err)
					require.Empty(t, findings)
				})
			}
		})
	}
}
