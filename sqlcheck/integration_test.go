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
	tbl := tabula.Define(reg, tabula.Table[item, int64, scope]{Name: schema + ".items", Order: "id ASC"})
	tbl.Statement("by_code", "SELECT "+tbl.Cols()+" FROM "+schema+".items WHERE code = $1")

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
}
