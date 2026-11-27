//go:build integration

package integration_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

func TestRegisteredEffectRoleRequirements(t *testing.T) {
	db, pool := databases(t)
	f := newEffectFixture(t, db, tabula.NewRegistry(tabula.RegistryConfig[struct{}]{}))
	ctx := t.Context()
	exec := func(query string) {
		t.Helper()
		_, err := db.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	exec("CREATE OR REPLACE FUNCTION " + f.schema + ".lookup(integer) RETURNS bigint LANGUAGE sql STABLE AS 'SELECT id FROM " + f.table + " WHERE id = $1'")
	snapshot := sqlcheck.RenderAllSnapshot(nil, f.reg.DefinedViews())
	defs, err := sqlcheck.ParseSnapshot(snapshot)
	require.NoError(t, err)
	work := sqlcheck.RoleStatements{}
	work.AddDefinitions(f.role, defs...)
	needs, err := work.Readiness(f.role)
	require.NoError(t, err)
	require.Len(t, needs.Routines, len(f.routines))
	items := f.view.Definition().StatementShapes["hold.global.data"].AnyColumnUpdate[0]
	require.Len(t, needs.AnyColumnUpdate, 1+len(f.statusLocks))
	require.Contains(t, needs.AnyColumnUpdate, items)
	missing, err := sqlcheck.CheckReadiness(ctx, db, f.role, needs)
	require.NoError(t, err)
	require.Empty(t, missing)
	findings, err := sqlcheck.CheckPolicyCoverage(ctx, db, work)
	require.NoError(t, err)
	require.Len(t, findings, len(f.routines))
	for _, finding := range findings {
		require.Contains(t, finding, ".call: typed statement requires reviewed Access or NoAccess")
	}
	for i := range work[f.role] {
		stmt := &work[f.role][i]
		switch {
		case strings.HasSuffix(stmt.Name, "/small.call"):
			stmt.Access = []sqlcheck.PolicyAccess{{Table: f.table, Command: "SELECT"}}
		case len(stmt.Shape.Routines) > 0:
			stmt.NoAccess = true
		case stmt.Shape.Effects&tabula.EffectAdvisoryLock != 0:
			require.True(t, stmt.NoAccess)
		}
	}
	exec("ALTER TABLE " + f.table + " ENABLE ROW LEVEL SECURITY")
	exec("CREATE POLICY visible ON " + f.table + " FOR SELECT TO " + f.role + " USING (true)")
	findings, err = sqlcheck.CheckPolicyCoverage(ctx, db, work)
	require.NoError(t, err)
	require.Len(t, findings, len(f.row))
	for _, finding := range findings {
		require.Contains(t, finding, "UPDATE")
	}
	exec("CREATE POLICY lockable ON " + f.table + " FOR UPDATE TO " + f.role + " USING (true)")
	findings, err = sqlcheck.CheckPolicyCoverage(ctx, db, work)
	require.NoError(t, err)
	require.Empty(t, findings)
	_, err = sqlcheck.VerifyRoleStatements(ctx, db, work)
	require.ErrorIs(t, err, sqlcheck.ErrDescribeRequired)
	findings, err = sqlcheck.VerifyRoleStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), work)
	require.NoError(t, err)
	require.Empty(t, findings)
	const alias = `label FOR UPDATE OF "i"`
	quoted := sqlcheck.Statement{Name: "quoted", SQL: `SELECT "FOR".id AS "label FOR UPDATE OF ""i""" FROM ` + f.table + ` AS "FOR" FOR KEY SHARE OF "FOR" NOWAIT`,
		Shape: &tabula.StatementShape{Effects: tabula.EffectRead | tabula.EffectRowLock, Result: []tabula.ShapeField{{Name: alias, Type: "pg_catalog.int8"}}}}
	findings, err = sqlcheck.VerifyRoleStatements(ctx, db, sqlcheck.RoleStatements{f.role: {quoted}})
	require.NoError(t, err)
	require.Empty(t, findings)
	quoted.Shape.Result[0].Type = "pg_catalog.text"
	findings, err = sqlcheck.VerifyRoleStatements(ctx, db, sqlcheck.RoleStatements{f.role: {quoted}})
	require.NoError(t, err)
	require.Len(t, findings, 1)
	require.Contains(t, findings[0], "result types")
	exec("REVOKE UPDATE(note) ON " + f.table + " FROM " + f.role)
	findings, err = sqlcheck.VerifyRoleStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), work)
	require.NoError(t, err)
	require.Len(t, findings, len(f.row))
	for _, finding := range findings {
		require.Contains(t, finding, "42501")
	}
	exec("ALTER FUNCTION " + f.schema + ".lookup(integer) SECURITY DEFINER")
	missing, err = sqlcheck.CheckReadiness(ctx, db, f.role, needs)
	require.NoError(t, err)
	require.Len(t, missing, 2)
	require.True(t, slices.ContainsFunc(missing, func(m sqlcheck.MissingRequirement) bool {
		return m.Object == items && m.Privilege == "UPDATE ANY COLUMN"
	}))
	require.True(t, slices.ContainsFunc(missing, func(m sqlcheck.MissingRequirement) bool {
		return m.Object == f.view.Definition().StatementShapes["small.call"].Routines[0].Signature && strings.HasSuffix(m.Privilege, "SECURITY INVOKER")
	}))
}
