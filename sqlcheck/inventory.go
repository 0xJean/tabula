package sqlcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// SQLMethod matches the package-qualified receiver type (without pointer or
// generic arguments). Argument is the zero-based SQL position in a method call.
type SQLMethod struct {
	Receiver, Method string
	Argument         int
}

// InventoryConfig selects buildable Go packages and reviewed site exceptions.
// Patterns default to ./... in Dir. Tags use the Go build tag syntax.
type InventoryConfig struct {
	Dir      string
	Patterns []string
	Tags     string
	// Tests includes internal and external _test.go files in matching packages.
	Tests      bool
	Methods    []SQLMethod
	Exceptions map[string]string
}

// SQLSite describes a static or dynamic call. ID is independent of line numbers.
type SQLSite struct {
	ID, File         string
	Line             int
	Receiver, Method string
	SQL              string `json:",omitempty"`
	Dynamic          bool   `json:",omitempty"`
	Exception        string `json:",omitempty"`
}

// SQLInventory retains reviewed sites and reports unreviewed dynamic SQL and stale exceptions.
type SQLInventory struct {
	Sites    []SQLSite
	Findings []string
}

type buildPackage struct {
	ImportPath, Dir, Export string
	CompiledGoFiles         []string
	ForTest                 string
	DepOnly                 bool
	ImportMap               map[string]string
}

// CollectSQL resolves Go types and constants using the installed Go toolchain,
// including aliases and imported constants. Escaping method values are dynamic
// sites. Run it once per build configuration whose SQL belongs in the inventory.
func CollectSQL(ctx context.Context, cfg InventoryConfig) (SQLInventory, error) {
	var out SQLInventory
	methods := make(map[[2]string]SQLMethod)
	for _, m := range cfg.Methods {
		k := [2]string{m.Receiver, m.Method}
		if m.Receiver == "" || m.Method == "" || m.Argument < 0 {
			return out, fmt.Errorf("invalid SQL method %+v", m)
		}
		if _, ok := methods[k]; ok {
			return out, fmt.Errorf("duplicate SQL method %v", k)
		}
		methods[k] = m
	}
	if len(methods) == 0 {
		return out, fmt.Errorf("specify at least one SQL method")
	}
	root, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return out, err
	}
	args := []string{"list", "-deps", "-export", "-compiled", "-json=ImportPath,Dir,Export,CompiledGoFiles,ForTest,DepOnly,ImportMap"}
	if cfg.Tests {
		args = append(args, "-test")
	}
	if cfg.Tags != "" {
		args = append(args, "-tags", cfg.Tags)
	}
	patterns := cfg.Patterns
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	for _, p := range patterns {
		if strings.HasPrefix(p, "-") {
			return out, fmt.Errorf("invalid package pattern %q", p)
		}
	}
	cmd := exec.CommandContext(ctx, "go", append(args, patterns...)...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("go list: %w: %s", err, stderr.String())
	}
	exports := map[string]string{}
	var selected []buildPackage
	dec := json.NewDecoder(bytes.NewReader(output))
	for dec.More() {
		var p buildPackage
		if err := dec.Decode(&p); err != nil {
			return out, err
		}
		exports[p.ImportPath] = p.Export
		if !p.DepOnly && len(p.CompiledGoFiles) > 0 {
			selected = append(selected, p)
		}
	}
	if len(selected) == 0 {
		return out, fmt.Errorf("no packages match %v", patterns)
	}
	seen := map[string]bool{}
	for _, p := range selected {
		sites, err := p.inventory(ctx, root, exports, methods)
		if err != nil {
			return out, err
		}
		for _, site := range sites {
			reason, ok := cfg.Exceptions[site.ID]
			if ok && strings.TrimSpace(reason) == "" {
				return out, fmt.Errorf("exception %s needs a reason", site.ID)
			}
			site.Exception = reason
			seen[site.ID] = true
			if site.Dynamic && !ok {
				out.Findings = append(out.Findings, "unreviewed dynamic SQL: "+site.ID)
			}
			out.Sites = append(out.Sites, site)
		}
	}
	for id := range cfg.Exceptions {
		if !seen[id] {
			out.Findings = append(out.Findings, "stale SQL exception: "+id)
		}
	}
	slices.SortFunc(out.Sites, func(a, b SQLSite) int { return strings.Compare(a.ID, b.ID) })
	slices.Sort(out.Findings)
	return out, nil
}

func (p buildPackage) inventory(ctx context.Context, root string, exports map[string]string, methods map[[2]string]SQLMethod) ([]SQLSite, error) {
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range p.CompiledGoFiles {
		if !filepath.IsAbs(name) {
			name = filepath.Join(p.Dir, name)
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		if mapped := p.ImportMap[path]; mapped != "" {
			path = mapped
		}
		return os.Open(exports[path])
	})}
	arch := os.Getenv("GOARCH")
	if arch == "" {
		arch = runtime.GOARCH
	}
	conf.Sizes = types.SizesFor("gc", arch)
	path, _, _ := strings.Cut(p.ImportPath, " [")
	if _, err := conf.Check(path, fset, files, info); err != nil {
		return nil, err
	}
	var sites []SQLSite
	counts := map[string]int{}
	for _, file := range files {
		if p.ForTest != "" && !strings.HasSuffix(fset.PositionFor(file.Pos(), false).Filename, "_test.go") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		bindings := constantLocals(file, info)
		calls := map[*ast.SelectorExpr]*ast.CallExpr{}
		ast.Inspect(file, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if s, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr); ok {
					calls[s] = c
				}
			}
			return true
		})
		for _, decl := range file.Decls {
			owner := "init"
			if f, ok := decl.(*ast.FuncDecl); ok {
				owner = f.Name.Name
				if fn, ok := info.Defs[f.Name].(*types.Func); ok {
					owner = fn.FullName()
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				s, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				sel := info.Selections[s]
				if sel == nil || sel.Kind() == types.FieldVal {
					return true
				}
				m, ok := methods[[2]string{receiverName(sel.Recv()), s.Sel.Name}]
				if !ok {
					sig := sel.Obj().Type().(*types.Signature)
					if recv := sig.Recv(); recv != nil {
						m, ok = methods[[2]string{receiverName(recv.Type()), s.Sel.Name}]
					}
				}
				if !ok {
					return true
				}
				pos := fset.PositionFor(s.Pos(), true)
				rel, _ := filepath.Rel(root, pos.Filename)
				site := SQLSite{File: filepath.ToSlash(rel), Line: pos.Line, Method: m.Method, Receiver: m.Receiver, Dynamic: true}
				var expr ast.Expr = s
				if c := calls[s]; c != nil {
					i := m.Argument
					if sel.Kind() == types.MethodExpr {
						i++
					}
					if i < len(c.Args) {
						expr = c.Args[i]
						value := info.Types[expr].Value
						if id, ok := ast.Unparen(expr).(*ast.Ident); ok && value == nil {
							value = bindings[info.Uses[id]]
						}
						if value != nil && value.Kind() == constant.String {
							site.SQL = constant.StringVal(value)
							site.Dynamic = false
						}
					}
				}
				var b bytes.Buffer
				_ = format.Node(&b, fset, expr)
				digest := sha256.Sum256([]byte(owner + "\n" + m.Receiver + "." + m.Method + "\n" + b.String() + "\n" + site.SQL))
				base := fmt.Sprintf("%s#%x", site.File, digest[:12])
				counts[base]++
				site.ID = fmt.Sprintf("%s-%d", base, counts[base])
				sites = append(sites, site)
				return true
			})
		}
	}
	return sites, nil
}

func receiverName(t types.Type) string {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	if n, ok := t.(*types.Named); ok && n.Obj().Pkg() != nil {
		return n.Obj().Pkg().Path() + "." + n.Obj().Name()
	}
	return ""
}

// Only a local initialized once with a constant, never reassigned or
// addressed, can be substituted for its expression.
func constantLocals(f *ast.File, info *types.Info) map[types.Object]constant.Value {
	out := map[types.Object]constant.Value{}
	seen := map[types.Object]bool{}
	record := func(e ast.Expr, v constant.Value) {
		id, ok := ast.Unparen(e).(*ast.Ident)
		if !ok {
			return
		}
		obj := info.ObjectOf(id)
		if obj == nil || obj.Pkg() == nil || obj.Parent() == obj.Pkg().Scope() {
			return
		}
		if seen[obj] {
			delete(out, obj)
		} else {
			out[obj] = v
		}
		seen[obj] = true
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, e := range x.Lhs {
				var v constant.Value
				if x.Tok == token.DEFINE && len(x.Rhs) == len(x.Lhs) {
					v = info.Types[x.Rhs[i]].Value
				}
				record(e, v)
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				var v constant.Value
				if len(x.Values) == len(x.Names) {
					v = info.Types[x.Values[i]].Value
				}
				record(id, v)
			}
		case *ast.RangeStmt:
			if x.Key != nil {
				record(x.Key, nil)
			}
			if x.Value != nil {
				record(x.Value, nil)
			}
		case *ast.UnaryExpr:
			if x.Op == token.AND {
				record(x.X, nil)
			}
		}
		return true
	})
	return out
}
