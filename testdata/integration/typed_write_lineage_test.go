//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

func TestWriteCastResultDescription(t *testing.T) {
	db, pool := databases(t)
	ctx, schema := t.Context(), typesSchema(t, db)
	_, err := db.ExecContext(ctx, "CREATE DOMAIN "+schema+`.kind AS text;
CREATE TABLE `+schema+`.items(id bigint PRIMARY KEY, rank int NOT NULL, label `+schema+`.kind NOT NULL);
INSERT INTO `+schema+`.items VALUES(1,0,'note')`)
	require.NoError(t, err)
	reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
	table := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".items", RelationScope: q.Global()})
	kind := q.CatalogType[string](schema, "kind", nil)
	src := tplan.MapTable(table, tplan.ColumnType("label", kind)).As("i")
	id, rank, label := tplan.FieldOf(src.Rel(), "id", tplan.Int8()), tplan.FieldOf(src.Rel(), "rank", tplan.Int4()), tplan.FieldOf(src.Rel(), "label", kind)
	value := tplan.Input("rank", tplan.Int4())
	type result struct {
		Value int64 `db:"value"`
	}
	out := q.DefineView(reg, q.ViewSpec[result]{Name: "entries"})
	plan := tplan.DefineReturning(out, "cast", tplan.WithReturning(tplan.UpdateOf(src).Set(tplan.Assign(rank, value.Expr())).AllRows(),
		tplan.Project(tplan.Output(out, "value", tplan.Int8()), tplan.Cast(tplan.Int8(), id.Expr()))))
	type domainResult struct {
		Value string `db:"value"`
	}
	domainOut := q.DefineView(reg, q.ViewSpec[domainResult]{Name: "groups"})
	domainPlan := tplan.DefineReturning(domainOut, "domain", tplan.WithReturning(tplan.UpdateOf(src).Set(tplan.Assign(rank, value.Expr())).AllRows(),
		tplan.Project(tplan.Output(domainOut, "value", kind), label.Expr())))
	def := out.Definition()
	domainDef := domainOut.Definition()
	require.Contains(t, def.Statements, "cast.global.write")
	require.Contains(t, def.StatementShapes, "cast.global.write")
	require.Contains(t, domainDef.Statements, "domain.global.write")
	require.Contains(t, domainDef.StatementShapes, "domain.global.write")
	shape := def.StatementShapes["cast.global.write"]
	require.Equal(t, []q.ResultLineage{{CastType: `"pg_catalog"."int8"`}}, shape.ResultLineage)
	require.Equal(t, []q.ResultLineage{{Relation: `"` + schema + `"."items"`, Column: "label"}}, domainDef.StatementShapes["domain.global.write"].ResultLineage)
	verification, err := pgxdriver.SQLVerifier(pool).BeginVerification(ctx)
	require.NoError(t, err)
	defer func() { _ = verification.Rollback(context.WithoutCancel(ctx)) }()
	description, err := verification.Describe(ctx, "items_cast", def.Statements["cast.global.write"])
	require.NoError(t, err)
	require.Len(t, description.Fields, 1)
	require.Equal(t, "value", description.Fields[0].Name)
	var int8OID, textOID uint32
	require.NoError(t, verification.Scan(ctx, "SELECT 'pg_catalog.int8'::regtype::oid, 'pg_catalog.text'::regtype::oid", nil, &int8OID, &textOID))
	require.Equal(t, int8OID, description.Fields[0].TypeOID)
	description, err = verification.Describe(ctx, "items_domain", domainDef.Statements["domain.global.write"])
	require.NoError(t, err)
	require.Len(t, description.Fields, 1)
	require.Equal(t, textOID, description.Fields[0].TypeOID)
	require.NotZero(t, description.Fields[0].TableOID)
	require.Equal(t, int16(3), description.Fields[0].Attribute)
	require.NoError(t, verification.Rollback(ctx))
	findings, err := sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), []q.Definition{q.Definition(def), q.Definition(domainDef)}, "")
	require.NoError(t, err)
	require.Empty(t, findings)
	var current int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT rank FROM "+schema+".items WHERE id=1").Scan(&current))
	require.Zero(t, current)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				rows, err := plan.Bind(s).All(ctx, tplan.BindValue(value, int32(1)))
				require.NoError(t, err)
				require.Equal(t, []result{{Value: 1}}, rows)
				domainRows, err := domainPlan.Bind(s).All(ctx, tplan.BindValue(value, int32(1)))
				require.NoError(t, err)
				require.Equal(t, []domainResult{{Value: "note"}}, domainRows)
				return nil
			}))
		})
	}
}
