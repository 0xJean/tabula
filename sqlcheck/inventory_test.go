package sqlcheck

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInventoryRoleAssignments(t *testing.T) {
	inv := SQLInventory{Sites: []SQLSite{{ID: "a", SQL: "SELECT 1"}, {ID: "b", Dynamic: true, Exception: "finite variants"}, {ID: "c", Dynamic: true, Exception: "covered separately"}}}
	bindings := map[string]SQLBinding{"a": {Roles: []string{"r", "w"}}, "b": {Roles: []string{"r"}, Variants: []Statement{{Name: "case", SQL: "SELECT 2"}}}, "c": {Skip: true}}
	work, err := inv.BindRoles(bindings)
	require.NoError(t, err)
	require.Len(t, work["r"], 2)
	require.Len(t, work["w"], 1)
	require.Equal(t, "b/case", work["r"][1].Name)
	bindings["d"] = SQLBinding{}
	_, err = inv.BindRoles(bindings)
	require.ErrorContains(t, err, "stale")
	delete(bindings, "d")
	delete(bindings, "b")
	_, err = inv.BindRoles(bindings)
	require.ErrorContains(t, err, "missing")
	bindings["b"] = SQLBinding{Roles: []string{"r"}}
	_, err = inv.BindRoles(bindings)
	require.ErrorContains(t, err, "variants")
	bindings["a"] = SQLBinding{Skip: true}
	_, err = inv.BindRoles(bindings)
	require.ErrorContains(t, err, "reviewed exception")
}

func TestCollectSQL(t *testing.T) {
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	write := func(name, data string) {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
		require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	}
	write("go.mod", "module example.test/inventory\n\ngo 1.26\n")
	write("api/session.go", `package api
 type Session[T any] struct{}
 func (*Session[T]) Send(n int, sql string){}
 const Query = "SELECT "`)
	src := `package calls
 import "example.test/inventory/api"
 type S = api.Session[int]
 type Wrapped struct{ *S }
 type Unrelated struct{}
 func (Unrelated) Send(int,string){}
 func calls(s *S,w Wrapped,other Unrelated, dynamic string) {
  s.Send(1, api.Query + "1")
  fixed := "SELECT 2"; w.Send(1,fixed)
  (*S).Send(s,1,"SELECT 3")
  s.Send(1,dynamic)
  nonconstant:="SELECT 99"; nonconstant=dynamic; s.Send(1,nonconstant)
  mutable:="SELECT 100"; _=&mutable; s.Send(1,mutable)
  f:=s.Send; f(1,dynamic)
  other.Send(1,"unrelated")
 }
 `
	write("calls.go", src)
	write("calls_test.go", `package calls
 func testUse(s *S){s.Send(1,"SELECT 4")}
 `)
	write("external_test.go", `package calls_test
 import "example.test/inventory/api"
 func external(s *api.Session[int]){s.Send(1,"SELECT 5")}
 `)
	write("extra.go", "//go:build extra\n\npackage calls\nfunc extra(s *S){s.Send(1,\"SELECT 6\")}\n")
	cfg := InventoryConfig{Dir: dir, Methods: []SQLMethod{{"example.test/inventory/api.Session", "Send", 1}}}
	report, err := CollectSQL(t.Context(), cfg)
	require.NoError(t, err)
	require.Len(t, report.Sites, 7)
	require.Len(t, report.Findings, 4)
	cfg.Exceptions = map[string]string{"removed-site": "old registration"}
	var sqls []string
	var ids []string
	for _, s := range report.Sites {
		require.Equal(t, "calls.go", s.File)
		ids = append(ids, s.ID)
		if s.Dynamic {
			cfg.Exceptions[s.ID] = "reviewed builder"
		} else {
			sqls = append(sqls, s.SQL)
		}
	}
	require.ElementsMatch(t, []string{"SELECT 1", "SELECT 2", "SELECT 3"}, sqls)
	write("calls.go", "\n\n"+src)
	after, err := CollectSQL(t.Context(), cfg)
	require.NoError(t, err)
	require.Equal(t, []string{"stale SQL exception: removed-site"}, after.Findings)
	for _, s := range after.Sites {
		require.Contains(t, ids, s.ID)
		if s.Dynamic {
			require.Equal(t, "reviewed builder", s.Exception)
		}
	}
	delete(cfg.Exceptions, "removed-site")
	cfg.Tests = true
	cfg.Tags = "extra"
	report, err = CollectSQL(t.Context(), cfg)
	require.NoError(t, err)
	require.Empty(t, report.Findings)
	require.Len(t, report.Sites, 10)
	for _, query := range []string{"SELECT 4", "SELECT 5", "SELECT 6"} {
		require.True(t, slices.ContainsFunc(report.Sites, func(s SQLSite) bool { return s.SQL == query }))
	}
	for id := range cfg.Exceptions {
		cfg.Exceptions[id] = ""
		break
	}
	_, err = CollectSQL(t.Context(), cfg)
	require.ErrorContains(t, err, "reason")
	cfg.Methods = append(cfg.Methods, cfg.Methods[0])
	_, err = CollectSQL(t.Context(), cfg)
	require.ErrorContains(t, err, "duplicate")
	cfg.Methods = cfg.Methods[:1]
	cfg.Patterns = []string{"-invalid"}
	_, err = CollectSQL(t.Context(), cfg)
	require.ErrorContains(t, err, "pattern")
	cfg.Patterns = []string{"./missing"}
	_, err = CollectSQL(t.Context(), cfg)
	require.ErrorContains(t, err, "go list")
	cfg.Methods = nil
	_, err = CollectSQL(t.Context(), cfg)
	require.Error(t, err)
	require.False(t, strings.Contains(strings.Join(ids, ","), dir))
}

func TestInventoryFieldsUseDeclaringStruct(t *testing.T) {
	t.Setenv("GOWORK", "off")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/fields\n\ngo 1.26\n"), 0600))
	const src = `package fields
type Display[T any] struct { Label T }
type Alias = Display[string]
type Details struct { *Alias }
type Panel struct { Details }
type Shadow struct { Panel; Label string }
type Copy Display[string]
func captions(d *Alias, p Panel, s Shadow, c Copy) {
	_ = d.Label
	_ = p.Label
	_ = Display[int]{Label: 1}
	_ = s.Label
	_ = c.Label
	_ = Copy{Label: ""}
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fields.go"), []byte(src), 0600))
	cfg := InventoryConfig{Dir: dir, CallTargets: []CallTarget{{
		Package: "example.test/fields", Receiver: "example.test/fields.Display", Name: "Label", Field: true, Category: "display",
	}}}
	inv, err := CollectSQL(t.Context(), cfg)
	require.NoError(t, err)
	var lines []int
	for _, site := range inv.CallSites {
		require.Equal(t, "example.test/fields.Display.Label", site.Symbol)
		require.Equal(t, "display", site.Category)
		lines = append(lines, site.Line)
	}
	require.Equal(t, []int{9, 10, 11}, lines)
}
