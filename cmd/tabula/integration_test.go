//go:build integration

package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck"
)

func TestCLICheckAgainstRealPostgres(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn, "TABULA_TEST_DSN is required for integration tests")
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()

	schema := fmt.Sprintf("tabula_cli_it_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE TABLE `+schema+`.widgets (id bigserial PRIMARY KEY, code text NOT NULL)`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })

	type widget struct {
		ID   int64  `db:"id,key,serial"`
		Code string `db:"code"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	tabula.Define(reg, tabula.Table[widget, int64, struct{}]{Name: schema + ".widgets"})

	dir := t.TempDir()
	goodSnap := filepath.Join(dir, "good.sql")
	require.NoError(t, os.WriteFile(goodSnap, []byte(sqlcheck.RenderSnapshot(reg.Defined(), "CLI integration snapshot")), 0o600))

	var out, errBuf bytes.Buffer
	code := run(ctx, []string{"check", "--dsn", dsn, "--snapshot", goodSnap}, &out, &errBuf)
	require.Equal(t, 0, code, "stderr: %s", errBuf.String())
	require.Contains(t, out.String(), "verified")

	badSnap := filepath.Join(dir, "bad.sql")
	require.NoError(t, os.WriteFile(badSnap, []byte("\n-- "+schema+".widgets\n-- broken\nSELECT missing_col FROM "+schema+".widgets;\n"), 0o600))
	out.Reset()
	errBuf.Reset()
	code = run(ctx, []string{"check", "--dsn", dsn, "--snapshot", badSnap}, &out, &errBuf)
	require.Equal(t, 1, code)
	require.Contains(t, errBuf.String(), "missing_col")
}
