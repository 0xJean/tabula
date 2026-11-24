package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/cmd/tabula/testdata/inventoryfixture"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

func TestCheckLoginRoleCLIValidation(t *testing.T) {
	t.Setenv("TABULA_DSN", "")
	for _, args := range [][]string{
		{"check-login-role"},
		{"check-login-role", "--dsn", "postgres://localhost/db"},
		{"check-login-role", "--bad-flag"},
	} {
		require.Equal(t, 2, run(t.Context(), args, io.Discard, io.Discard))
	}
	var out bytes.Buffer
	require.Equal(t, 0, run(t.Context(), []string{"check-login-role", "-h"}, &out, &out))
	require.Contains(t, out.String(), "-member-of")
}

func TestInventoryCLIWithComposedQuerySnapshot(t *testing.T) {
	dir := t.TempDir()
	writeJSON := func(name string, value any) string {
		t.Helper()
		path := filepath.Join(dir, name)
		data, err := json.Marshal(value)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0600))
		return path
	}
	cfg := writeJSON("config.json", sqlcheck.InventoryConfig{Dir: "testdata/inventoryfixture", AuditSecurityCalls: true})
	var out, diag bytes.Buffer
	require.Equal(t, 0, run(t.Context(), []string{"inventory", "--config", cfg}, &out, &diag), diag.String())

	var inv sqlcheck.SQLInventory
	require.NoError(t, json.Unmarshal(out.Bytes(), &inv))
	require.Empty(t, inv.Findings)
	require.Len(t, inv.Sites, 4)
	ids := map[string]string{}
	bindings := map[string]sqlcheck.SQLBinding{}
	for _, site := range inv.Sites {
		var role string
		var binding sqlcheck.SQLBinding
		switch site.QueryKey {
		case "list":
			require.Contains(t, []string{"notes_view", "labels_view"}, site.Definition)
			role = site.Definition + "_reader"
		case "list.recent":
			role = "recent_reader"
		case "countby:category":
			require.Empty(t, site.Definition, "rows are supplied by the caller")
			role = "counts_reader"
			binding.Definition = "records"
		default:
			t.Fatalf("unexpected site %+v", site)
		}
		ids[role] = site.ID
		binding.Roles = []string{role}
		bindings[site.ID] = binding
	}
	bindPath := writeJSON("bindings.json", bindings)
	snapPath := filepath.Join(dir, "statements.sql")
	bind := func(tables []tabula.Definition, views []tabula.ViewDefinition, wantCode int) sqlcheck.RoleStatements {
		t.Helper()
		require.NoError(t, os.WriteFile(snapPath, []byte(sqlcheck.RenderAllSnapshot(tables, views)), 0600))
		out.Reset()
		diag.Reset()
		code := run(t.Context(), []string{"inventory", "--config", cfg, "--bindings", bindPath, "--snapshot", snapPath}, &out, &diag)
		require.Equal(t, wantCode, code, diag.String())
		var work sqlcheck.RoleStatements
		require.NoError(t, json.Unmarshal(out.Bytes(), &work))
		return work
	}

	reg := inventoryfixture.Registry()
	tables, views := reg.Defined(), reg.DefinedViews()
	work := bind(tables, views, 0)
	require.Len(t, work, 4)
	for _, group := range []struct{ role, view, query, relation string }{
		{"notes_view_reader", "notes_view", "list", "notes"},
		{"labels_view_reader", "labels_view", "list", "labels"},
		{"recent_reader", "notes_view", "list.recent", "recent_notes"},
	} {
		stmts := work[group.role]
		require.Len(t, stmts, 3)
		for i, suffix := range []string{"count", "find", "page"} {
			require.Equal(t, ids[group.role]+"/"+group.view+"/"+group.query+"."+suffix, stmts[i].Name)
			require.Contains(t, stmts[i].SQL, "FROM "+group.relation)
		}
	}
	require.Len(t, work["counts_reader"], 1)
	require.Equal(t, ids["counts_reader"]+"/records/count_by_category", work["counts_reader"][0].Name)

	require.Nil(t, bind(tables, views[:1], 1))
	require.Contains(t, diag.String(), ids["labels_view_reader"])
	require.Contains(t, diag.String(), "labels_view")
	require.Nil(t, bind(nil, views, 1))
	require.Contains(t, diag.String(), ids["counts_reader"])
	require.Contains(t, diag.String(), "records")
}

func TestCheckLoginRoleCLI_Integration(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	if dsn == "" {
		t.Skip("TABULA_TEST_DSN not set")
	}
	ctx := t.Context()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	schema := "tabula_cli_lr_" + suffix
	login := "tabula_cli_login_" + suffix
	worker := "tabula_cli_worker_" + suffix
	for _, q := range []string{
		"CREATE SCHEMA " + schema,
		"REVOKE CREATE ON SCHEMA " + schema + " FROM PUBLIC",
		"CREATE ROLE " + worker + " NOLOGIN",
		"CREATE ROLE " + login + " LOGIN NOINHERIT",
		"GRANT " + worker + " TO " + login,
	} {
		_, err := db.ExecContext(ctx, q)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		_, _ = db.Exec("DROP ROLE IF EXISTS " + login)
		_, _ = db.Exec("DROP ROLE IF EXISTS " + worker)
	})

	var out, diag bytes.Buffer
	code := run(ctx, []string{"check-login-role", "--dsn", dsn, "--role", login, "--member-of", worker, "--schemas", schema}, &out, &diag)
	require.Equal(t, 0, code, diag.String())
	require.Contains(t, out.String(), "passed hygiene checks")

	// Failing case returns exit code 1.
	out.Reset()
	diag.Reset()
	code = run(ctx, []string{"check-login-role", "--dsn", dsn, "--role", login, "--member-of", "other_role", "--schemas", schema}, &out, &diag)
	require.Equal(t, 1, code)
	require.Contains(t, diag.String(), "login role violations found")
}
