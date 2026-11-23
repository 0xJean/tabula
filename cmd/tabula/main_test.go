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
		require.Contains(t, errBuf.String(), "usage: tabula check")
	})

	t.Run("help", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		require.Equal(t, 0, run(t.Context(), []string{"--help"}, &out, &errBuf))
		require.Contains(t, out.String(), "usage: tabula check")
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
