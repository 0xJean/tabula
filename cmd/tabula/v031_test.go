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
	t.Setenv("GOWORK", "off")
	repoRoot, err := filepath.Abs("../..")
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module sample.test/q\n\ngo 1.26.0\n\nrequire github.com/0xJean/tabula v0.3.0\n\nreplace github.com/0xJean/tabula => "+filepath.ToSlash(repoRoot)+"\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "q.go"), []byte(`package q
import "github.com/0xJean/tabula"
type row struct { ID int64 `+"`db:\"id\"`"+` }
func def(r *tabula.Registry[struct{}]) {
	v := tabula.DefineView(r, tabula.ViewSpec[row]{Name: "items_v"})
	_ = v.DefineQuery("items.list", tabula.QuerySpec{Select: "SELECT id FROM items", MaxLimit: 10})
}
`), 0600))

	// Generate matching C3 snapshot.
	type row struct {
		ID int64 `db:"id"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	v := tabula.DefineView(reg, tabula.ViewSpec[row]{Name: "items_v"})
	v.DefineQuery("items.list", tabula.QuerySpec{
		Select: "SELECT id FROM items",
		Order: tabula.QueryOrderSpec{
			Default:          "id",
			DefaultDirection: tabula.SortAsc,
			Sorts:            []tabula.QuerySort{{Key: "id", Expr: "id", Column: "id"}},
		},
		MaxLimit: 10,
	})
	snapPath := filepath.Join(dir, "views.sql")
	require.NoError(t, os.WriteFile(snapPath, []byte(sqlcheck.RenderAllSnapshot(nil, reg.DefinedViews())), 0600))

	cfgPath := filepath.Join(dir, "cfg.json")
	cfgBytes, err := json.Marshal(sqlcheck.InventoryConfig{
		Dir:                dir,
		AuditSecurityCalls: true,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfgPath, cfgBytes, 0600))

	var out, diag bytes.Buffer
	require.Equal(t, 0, run(t.Context(), []string{"inventory", "--config", cfgPath}, &out, &diag), diag.String())
	var inv sqlcheck.SQLInventory
	require.NoError(t, json.Unmarshal(out.Bytes(), &inv))
	require.Len(t, inv.Sites, 1)
	require.Equal(t, "items.list", inv.Sites[0].QueryKey)

	bindPath := filepath.Join(dir, "bindings.json")
	bindBytes, err := json.Marshal(map[string]sqlcheck.SQLBinding{
		inv.Sites[0].ID: {Roles: []string{"app_reader"}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(bindPath, bindBytes, 0600))

	out.Reset()
	diag.Reset()
	require.Equal(t, 0, run(t.Context(), []string{"inventory", "--config", cfgPath, "--bindings", bindPath, "--snapshot", snapPath}, &out, &diag), diag.String())
	var work sqlcheck.RoleStatements
	require.NoError(t, json.Unmarshal(out.Bytes(), &work))
	require.NotEmpty(t, work["app_reader"])
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
