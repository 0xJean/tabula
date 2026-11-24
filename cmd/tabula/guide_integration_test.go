//go:build integration

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

// Read the published sample inputs so a stale field, command or expected result
// in the guide fails through the same entry point an application would use.
func TestAuthorizationGuide(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "AUTHORIZATION.md"))
	require.NoError(t, err)
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	t.Setenv("TABULA_DSN", dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	// Only substitute fixture identifiers so the guide runs unchanged in
	// effect while concurrent test runs can use the same disposable server.
	schema := fmt.Sprintf("tabula_guide_%d", time.Now().UnixNano())
	isolate := regexp.MustCompile(`\bdocs(_reader|_writer)?\b`)
	fixture := func(s string) string {
		return isolate.ReplaceAllString(s, schema+"${1}")
	}
	t.Cleanup(func() {
		_, err := db.Exec(fixture(`DROP SCHEMA IF EXISTS docs CASCADE; DROP ROLE IF EXISTS docs_reader, docs_writer`))
		require.NoError(t, err)
	})
	_, err = db.ExecContext(t.Context(), fixture(`
		CREATE ROLE docs_reader;
		CREATE ROLE docs_writer;
		CREATE SCHEMA docs;
		CREATE TABLE docs.notes(id integer PRIMARY KEY, scope text DEFAULT 'east');
		INSERT INTO docs.notes(id, scope) VALUES (1, 'east');
		GRANT USAGE ON SCHEMA docs TO docs_reader, docs_writer;
		GRANT SELECT(id) ON docs.notes TO docs_reader;
		GRANT INSERT(id), SELECT(id) ON docs.notes TO docs_writer;
		ALTER TABLE docs.notes ENABLE ROW LEVEL SECURITY;
		ALTER TABLE docs.notes FORCE ROW LEVEL SECURITY;
		CREATE POLICY read_note ON docs.notes FOR SELECT TO docs_reader USING (scope = current_setting('example.scope', true));
		CREATE POLICY add_note ON docs.notes FOR INSERT TO docs_writer WITH CHECK (true);
		CREATE POLICY scope_fence ON docs.notes AS RESTRICTIVE FOR ALL TO PUBLIC USING (true);
	`))
	require.NoError(t, err)

	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GOWORK", "off")
	require.NoError(t, os.WriteFile("go.mod", []byte("module example.org/notes\n\ngo 1.26.0\n"), 0o600))
	require.NoError(t, os.WriteFile("notes.go", []byte(fixture(`package notes
import ("context"; "database/sql")
func Read(ctx context.Context, tx *sql.Tx, id int) (*sql.Rows, error) {
	return tx.QueryContext(ctx, "SELECT id FROM docs.notes WHERE id = $1", id)
}
func Add(ctx context.Context, tx *sql.Tx, id int) (sql.Result, error) {
	return tx.ExecContext(ctx, "INSERT INTO docs.notes(id) VALUES($1)", id)
}
`)), 0o600))

	block := regexp.MustCompile("(?ms)^`([a-z][a-z0-9-]*\\.json)`\n\n```json\n(.*?)\n```$")
	configs := map[string]bool{}
	for _, match := range block.FindAllStringSubmatch(string(guide), -1) {
		name := match[1]
		require.NotContains(t, configs, name)
		configs[name] = false
		require.NoError(t, os.WriteFile(name, []byte(fixture(match[2])), 0o600))
	}
	require.Len(t, configs, strings.Count(string(guide), "```json\n"), "give every JSON block a filename")

	call := func(command, config string, code int, options ...string) []byte {
		t.Helper()
		require.Contains(t, configs, config)
		configs[config] = true
		var out, diag bytes.Buffer
		args := append([]string{command, "--config", config}, options...)
		require.Equal(t, code, run(t.Context(), args, &out, &diag), "%s: %s", command, diag.String())
		return out.Bytes()
	}
	var inventory sqlcheck.SQLInventory
	require.NoError(t, json.Unmarshal(call("inventory", "inventory.json", 0), &inventory))
	require.Len(t, inventory.Sites, 2)
	require.Empty(t, inventory.Findings)
	call("verify-roles", "workload.json", 0)
	call("grants", "privileges.json", 0)
	var minimum sqlcheck.Minimum
	require.NoError(t, json.Unmarshal(call("minimize", "trials.json", 0), &minimum))
	require.Len(t, minimum.Kept, 1)
	require.Equal(t, "id", minimum.Kept[0].Column)
	require.Contains(t, string(call("catalog", "scope.json", 0, "--section", "force-rls.json")), "notes")
	var classes []sqlcheck.PolicyClass
	require.NoError(t, json.Unmarshal(call("check-policies", "policies.json", 0), &classes))
	require.Len(t, classes, 3)
	var findings []string
	require.NoError(t, json.Unmarshal(call("check-coverage", "workload.json", 1), &findings))
	require.Equal(t, []string{fixture(`docs_writer/add-note: "docs"."notes" SELECT: no applicable permissive policy`)}, findings)
	call("test-rls", "behavior.json", 0)
	for name, ran := range configs {
		require.True(t, ran, "%s has no CLI example test", name)
	}
}
