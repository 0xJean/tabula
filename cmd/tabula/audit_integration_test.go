//go:build integration

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

func TestAuditCLI(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	t.Setenv("TABULA_DSN", dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	ns := fmt.Sprintf("tabula_cli_acl_%d", time.Now().UnixNano())
	role := ns + "_read"
	defer func() {
		_, _ = db.Exec("DROP SCHEMA IF EXISTS " + ns + " CASCADE")
		_, _ = db.Exec("DROP ROLE IF EXISTS " + role)
	}()
	for _, q := range []string{"CREATE ROLE " + role, "CREATE SCHEMA " + ns, "CREATE TABLE " + ns + ".items(id int)", "GRANT USAGE ON SCHEMA " + ns + " TO " + role, "GRANT SELECT, DELETE ON " + ns + ".items TO " + role, "ALTER TABLE " + ns + ".items ENABLE ROW LEVEL SECURITY", "CREATE POLICY read ON " + ns + ".items FOR SELECT TO " + role + " USING (true)"} {
		_, err := db.Exec(q)
		require.NoError(t, err)
	}
	dir := t.TempDir()
	call := func(code int, cmd string, data any, opts ...string) string {
		t.Helper()
		b, err := json.Marshal(data)
		require.NoError(t, err)
		cfg := filepath.Join(dir, "command.json")
		require.NoError(t, os.WriteFile(cfg, b, 0600))
		var out, diag bytes.Buffer
		rc := run(t.Context(), append([]string{cmd, "--config", cfg}, opts...), &out, &diag)
		require.Equal(t, code, rc, "%s %s", cmd, diag.String())
		return out.String()
	}
	scope := sqlcheck.CatalogScope{Schemas: []string{ns}, Roles: []string{role}}
	work := sqlcheck.RoleStatements{role: {{Name: "read", SQL: "SELECT id FROM " + ns + ".items"}}}
	call(0, "verify-roles", work)
	call(0, "check-coverage", work)
	call(1, "grants", sqlcheck.GrantManifest{CatalogScope: scope})
	out := call(0, "minimize", sqlcheck.MinimizeConfig{Role: role, Schemas: scope.Schemas, Statements: work[role]})
	require.Contains(t, out, "REVOKE DELETE")
	call(0, "check-policies", map[string]any{"Schemas": scope.Schemas, "Roles": scope.Roles, "Rules": []sqlcheck.PolicyRule{{Name: "read", Roles: scope.Roles}}})
	call(0, "test-rls", map[string]any{"Roles": scope.Roles, "Scopes": map[string]any{"default": map[string]string{}}, "Statements": map[string]any{"read": map[string]string{"SQL": work[role][0].SQL}}, "Cases": []any{map[string]any{"Role": role, "Scope": "default", "Statement": "read", "Rows": []any{}}}})
	large := "9223372036854775807"
	call(0, "test-rls", sqlcheck.RLSMatrix{Roles: scope.Roles, Scopes: map[string]map[string]string{"default": {}},
		Statements: map[string]sqlcheck.BehaviorStatement{"number": {SQL: "SELECT $1::bigint", Args: []any{json.Number(large)}}},
		Cases:      []sqlcheck.BehaviorCase{{Role: role, Scope: "default", Statement: "number", Rows: [][]*string{{&large}}}},
	})
	call(1, "catalog", scope, "--against", dir)
	for _, s := range []string{"policies.json", "grants.json", "force-rls.json", "security-definers.json"} {
		snapshot := call(0, "catalog", scope, "--section", s)
		require.NoError(t, os.WriteFile(filepath.Join(dir, s), []byte(snapshot), 0600))
	}
	call(0, "catalog", scope, "--against", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "policies.json"), []byte("[]\n"), 0600))
	call(1, "catalog", scope, "--against", dir)
	call(2, "catalog", scope, "--section", "../bad")
	call(2, "minimize", map[string]any{"invalid": true})
}
