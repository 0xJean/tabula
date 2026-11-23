//go:build integration

package sqlcheck

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
)

type scope string

type item struct {
	ID        int64      `db:"id,key,serial"`
	Tenant    scope      `db:"tenant_id,tenant"`
	Code      string     `db:"code"`
	Note      *string    `db:"note"`
	Version   int        `db:"version,version"`
	DeletedAt *time.Time `db:"deleted_at,deleted"`
}

// TestVerifyAgainstPostgres needs TABULA_TEST_DSN pointing at a disposable
// database whose user may create schemas and roles. It fails, never skips,
// when the variable is missing.
func TestVerifyAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn, "TABULA_TEST_DSN is required for integration tests")
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()

	schema := fmt.Sprintf("tabula_it_%d", time.Now().UnixNano())
	role, app := schema+"_reader", schema+"_app"
	for _, q := range []string{
		`CREATE SCHEMA ` + schema,
		`CREATE TABLE ` + schema + `.items (id bigserial PRIMARY KEY, tenant_id text NOT NULL, code text NOT NULL, note text, version integer NOT NULL DEFAULT 1, deleted_at timestamptz)`,
		`CREATE ROLE ` + role + ` NOLOGIN`,
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role,
		`GRANT SELECT (id, tenant_id, code) ON ` + schema + `.items TO ` + role,
		`CREATE ROLE ` + app + ` NOLOGIN`,
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + app,
		`GRANT SELECT (id, tenant_id, code, note, version, deleted_at), INSERT (tenant_id, code, note, version), UPDATE (code, note, version, deleted_at) ON ` + schema + `.items TO ` + app,
		`GRANT USAGE ON SEQUENCE ` + schema + `.items_id_seq TO ` + app,
	} {
		_, err := db.ExecContext(ctx, q)
		require.NoError(t, err, q)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_, _ = db.Exec(`DROP ROLE ` + role)
		_, _ = db.Exec(`DROP ROLE ` + app)
	})

	reg := tabula.NewRegistry(tabula.RegistryConfig[scope]{TenantID: func(s scope) any { return string(s) }})
	tbl := tabula.Define(reg, tabula.TableSpec[item, int64]{Name: schema + ".items", Order: "id ASC"})
	tbl.Statement("by_code", "SELECT "+tbl.Cols()+" FROM "+schema+".items WHERE code = $1")
	tbl.DefineUpsert("upsert_id", tabula.UpsertSpec{
		Conflict:  []string{"id"},
		Update:    []string{"code", "note", "version"},
		Returning: []string{"id"},
	})

	t.Run("snapshot round-trip verifies against real postgres", func(t *testing.T) {
		snap := RenderSnapshot(reg.Defined(), "Generated in integration test")
		parsed, err := ParseSnapshot(snap)
		require.NoError(t, err)
		failures, err := VerifyStatements(ctx, db, parsed, "")
		require.NoError(t, err)
		require.Empty(t, failures)
	})

	t.Run("schema matches the descriptor", func(t *testing.T) {
		families := DefaultTypeFamilies()
		families["sqlcheck.scope"] = []string{"text"}
		failures, err := VerifySchema(ctx, db, reg.Defined(), families)
		require.NoError(t, err)
		require.Empty(t, failures)
	})

	t.Run("every statement prepares as the owner", func(t *testing.T) {
		failures, err := VerifyStatements(ctx, db, reg.Defined(), "")
		require.NoError(t, err)
		require.Empty(t, failures)
	})

	t.Run("a broken statement is reported", func(t *testing.T) {
		defs := []tabula.Definition{{Name: schema + ".items", Statements: map[string]string{"bad": "SELECT missing FROM " + schema + ".items"}}}
		failures, err := VerifyStatements(ctx, db, defs, "")
		require.NoError(t, err)
		require.Len(t, failures, 1)
		require.Contains(t, failures[0], `column "missing" does not exist`)
	})

	t.Run("a role without column grants fails at EXPLAIN", func(t *testing.T) {
		defs := []tabula.Definition{{Name: schema + ".items", Statements: map[string]string{"note": "SELECT note FROM " + schema + ".items"}}}
		failures, err := VerifyStatements(ctx, db, defs, role)
		require.NoError(t, err)
		require.Len(t, failures, 1)
		require.Contains(t, failures[0], "permission denied")
	})

	t.Run("role grants match the derived needs", func(t *testing.T) {
		needs, _ := CollectPrivilegeNeeds(reg.Defined())
		missing, extra, err := CheckRolePrivileges(ctx, db, needs, app)
		require.NoError(t, err)
		require.Empty(t, missing)
		require.Empty(t, extra)
	})

	t.Run("a narrower role reports what is missing", func(t *testing.T) {
		needs, _ := CollectPrivilegeNeeds(reg.Defined())
		missing, extra, err := CheckRolePrivileges(ctx, db, needs, role)
		require.NoError(t, err)
		require.Contains(t, missing, schema+".items INSERT(code)")
		require.Contains(t, missing, schema+".items USAGE(seq id)")
		require.Empty(t, extra)
	})

	t.Run("views, materialized views, functions, delete privileges and RLS audit", func(t *testing.T) {
		for _, q := range []string{
			`CREATE VIEW ` + schema + `.active_items AS SELECT id, code FROM ` + schema + `.items WHERE deleted_at IS NULL`,
			`CREATE MATERIALIZED VIEW ` + schema + `.mv_items AS SELECT id, code FROM ` + schema + `.items`,
			`CREATE FUNCTION ` + schema + `.count_items(p_tenant text) RETURNS bigint LANGUAGE sql AS 'SELECT count(*) FROM ` + schema + `.items WHERE tenant_id = $1'`,
			`GRANT SELECT ON ` + schema + `.active_items, ` + schema + `.mv_items TO ` + app,
			`GRANT EXECUTE ON FUNCTION ` + schema + `.count_items(text) TO ` + app,
			`GRANT DELETE ON ` + schema + `.items TO ` + app,
			`REVOKE EXECUTE ON FUNCTION ` + schema + `.count_items(text) FROM PUBLIC`,
			`ALTER TABLE ` + schema + `.items ENABLE ROW LEVEL SECURITY`,
			`ALTER TABLE ` + schema + `.items FORCE ROW LEVEL SECURITY`,
			`CREATE POLICY items_tenant_scope ON ` + schema + `.items AS RESTRICTIVE FOR ALL TO PUBLIC USING (tenant_id = current_setting('app.current_tenant', true))`,
		} {
			_, err := db.ExecContext(ctx, q)
			require.NoError(t, err, q)
		}

		type summaryView struct {
			ID   int64  `db:"id,zero"`
			Code string `db:"code,zero"`
		}
		vReg := tabula.NewRegistry(tabula.RegistryConfig[scope]{TenantID: func(s scope) any { return string(s) }})
		v1 := tabula.DefineView(vReg, tabula.ViewSpec[summaryView]{Name: schema + ".active_items"})
		v1.Statement("all", "SELECT "+v1.Cols()+" FROM "+schema+".active_items")
		v2 := tabula.DefineView(vReg, tabula.ViewSpec[summaryView]{Name: schema + ".mv_items"})
		v2.Statement("all", "SELECT "+v2.Cols()+" FROM "+schema+".mv_items")

		// Verify regular view and materialized view schemas and statements.
		failures, err := VerifyViewSchema(ctx, db, vReg.DefinedViews(), DefaultTypeFamilies())
		require.NoError(t, err)
		require.Empty(t, failures)

		failures, err = VerifyViewStatements(ctx, db, vReg.DefinedViews(), app)
		require.NoError(t, err)
		require.Empty(t, failures)

		// RenderAllSnapshot includes both tables and views.
		allSnap := RenderAllSnapshot(reg.Defined(), vReg.DefinedViews(), "Combined snapshot")
		parsed, err := ParseSnapshot(allSnap)
		require.NoError(t, err)
		failures, err = VerifyStatements(ctx, db, parsed, app)
		require.NoError(t, err)
		require.Empty(t, failures)

		// DELETE and function EXECUTE privileges checked accurately.
		delReg := tabula.NewRegistry(tabula.RegistryConfig[scope]{TenantID: func(s scope) any { return string(s) }})
		delTbl := tabula.Define(delReg, tabula.TableSpec[item, int64]{Name: schema + ".items", AllowDelete: true})
		delTbl.RequiresFunction(schema + ".count_items(text)")
		delNeeds, _ := CollectPrivilegeNeeds(delReg.Defined())
		missing, _, err := CheckRolePrivileges(ctx, db, delNeeds, app)
		require.NoError(t, err)
		require.Empty(t, missing)

		missingNarrow, _, err := CheckRolePrivileges(ctx, db, delNeeds, role)
		require.NoError(t, err)
		require.Contains(t, missingNarrow, schema+".items DELETE")
		require.Contains(t, missingNarrow, "EXECUTE("+schema+".count_items(text))")

		// Generic RLS audit passes when ENABLE + FORCE + RESTRICTIVE are present.
		rlsFails, err := CheckRLS(ctx, db, RLSExpectation{
			Schema:               schema,
			Tables:               []string{"items"},
			RequireForce:         true,
			RequireRestrictive:   true,
			ForbiddenPolicyRoles: []string{role},
		})
		require.NoError(t, err)
		require.Empty(t, rlsFails)

		// Reports a table without RLS or FORCE.
		_, err = db.ExecContext(ctx, `CREATE TABLE `+schema+`.unprotected (id int PRIMARY KEY)`)
		require.NoError(t, err)
		rlsFails, err = CheckRLS(ctx, db, RLSExpectation{
			Schema:             schema,
			Tables:             []string{"unprotected"},
			RequireForce:       true,
			RequireRestrictive: true,
		})
		require.NoError(t, err)
		require.Equal(t, []string{schema + ".unprotected: ROW LEVEL SECURITY is not enabled"}, rlsFails)
	})
}
