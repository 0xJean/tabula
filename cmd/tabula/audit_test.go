package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

func TestSourceInventoryCLI(t *testing.T) {
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	var out, diag bytes.Buffer
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module sample.test/p\n\ngo 1.26\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s.go"), []byte("package p\n type S struct{}\n func(S) Q(string){}\nfunc query(s S){s.Q(\"SELECT 1\")}\n"), 0600))
	data := `{"Methods":[{"Receiver":"sample.test/p.S","Method":"Q","Argument":0}],"Dir":"` + filepath.ToSlash(dir) + `"}`
	require.NoError(t, os.WriteFile(cfg, []byte(data), 0600))
	require.Equal(t, 0, run(t.Context(), []string{"inventory", "--config", cfg}, &out, &diag), diag.String())
	require.Contains(t, out.String(), "SELECT 1")
	var inv sqlcheck.SQLInventory
	require.NoError(t, json.Unmarshal(out.Bytes(), &inv))
	bindings := filepath.Join(dir, "bindings.json")
	b, err := json.Marshal(map[string]sqlcheck.SQLBinding{inv.Sites[0].ID: {Roles: []string{"reader"}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(bindings, b, 0600))
	out.Reset()
	require.Equal(t, 0, run(t.Context(), []string{"inventory", "--config", cfg, "--bindings", bindings}, &out, &diag), diag.String())
	var work sqlcheck.RoleStatements
	require.NoError(t, json.Unmarshal(out.Bytes(), &work))
	require.Equal(t, "SELECT 1", work["reader"][0].SQL)
	for _, args := range [][]string{{"inventory"}, {"inventory", "--bad"}, {"inventory", "--config", cfg, "extra"}, {"inventory", "--config", cfg + ".missing"}} {
		require.Equal(t, 2, run(t.Context(), args, io.Discard, io.Discard))
	}
	_, err = decodeConfig[struct{}]([]byte("{} {}"))
	require.Error(t, err)
}
