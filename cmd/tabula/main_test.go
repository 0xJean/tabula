package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCLISubcommandValidation(t *testing.T) {
	t.Run("no_args", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		require.Equal(t, 2, run(t.Context(), nil, &out, &errBuf))
		require.Contains(t, errBuf.String(), "tabula check")
		require.Contains(t, errBuf.String(), "tabula check-rls")
	})

	t.Run("help", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		require.Equal(t, 0, run(t.Context(), []string{"--help"}, &out, &errBuf))
		require.Contains(t, out.String(), "tabula check")
		require.Contains(t, out.String(), "tabula check-rls")
	})

	t.Run("missing_dsn", func(t *testing.T) {
		t.Setenv("TABULA_DSN", "")
		var out, errBuf bytes.Buffer
		require.Equal(t, 2, run(t.Context(), []string{"check", "--snapshot", "a.sql"}, &out, &errBuf))
		require.Contains(t, errBuf.String(), "--dsn")
	})

	t.Run("missing_snapshot", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		require.Equal(t, 2, run(t.Context(), []string{"check", "--dsn", "postgres://localhost/x"}, &out, &errBuf))
		require.Contains(t, errBuf.String(), "--snapshot")
	})

	t.Run("empty_snapshot_file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.sql")
		require.NoError(t, os.WriteFile(path, []byte("-- header only\n"), 0o600))
		var out, errBuf bytes.Buffer
		require.Equal(t, 2, run(t.Context(), []string{"check", "--dsn", "postgres://localhost/x", "--snapshot", path}, &out, &errBuf))
		require.Contains(t, errBuf.String(), "contains no statements")
	})
}

func TestCommandHelp(t *testing.T) {
	const privateDSN = "private-connection-value"
	t.Setenv("TABULA_DSN", privateDSN)
	for _, c := range commands() {
		t.Run(c.name, func(t *testing.T) {
			var out, diag bytes.Buffer
			require.Equal(t, 0, run(t.Context(), []string{c.name, "-h"}, &out, &diag))
			require.Contains(t, diag.String(), "Usage of "+c.name+":")
			require.NotContains(t, diag.String(), privateDSN)
			diag.Reset()
			require.Equal(t, 2, run(t.Context(), []string{c.name, "--unknown-option"}, &out, &diag))
			require.NotContains(t, diag.String(), privateDSN)
		})
	}
}
