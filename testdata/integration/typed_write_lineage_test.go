//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
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
	src := q.MapTable(table, q.ColumnType("label", kind)).As("i")
	id, rank, label := q.FieldOf(src.Rel(), "id", q.Int8()), q.FieldOf(src.Rel(), "rank", q.Int4()), q.FieldOf(src.Rel(), "label", kind)
	value := q.Input("rank", q.Int4())
	type result struct {
		Value int64 `db:"value"`
	}
	out := q.DefineView(reg, q.ViewSpec[result]{Name: "entries"})
	plan := q.DefineReturning(out, "cast", q.WithReturning(q.UpdateOf(src).Set(q.Assign(rank, value.Expr())).AllRows(),
		q.Project(q.Output(out, "value", q.Int8()), q.Cast(q.Int8(), id.Expr()))))
	type domainResult struct {
		Value string `db:"value"`
	}
	domainOut := q.DefineView(reg, q.ViewSpec[domainResult]{Name: "groups"})
	domainPlan := q.DefineReturning(domainOut, "domain", q.WithReturning(q.UpdateOf(src).Set(q.Assign(rank, value.Expr())).AllRows(),
		q.Project(q.Output(domainOut, "value", kind), label.Expr())))
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
				rows, err := plan.Bind(s).All(ctx, q.BindValue(value, int32(1)))
				require.NoError(t, err)
				require.Equal(t, []result{{Value: 1}}, rows)
				domainRows, err := domainPlan.Bind(s).All(ctx, q.BindValue(value, int32(1)))
				require.NoError(t, err)
				require.Equal(t, []domainResult{{Value: "note"}}, domainRows)
				return nil
			}))
		})
	}
}
