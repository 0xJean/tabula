package sqlcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
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

const (
	tabulaPkgPath    = "github.com/0xJean/tabula"
	stdsqlPkgPath    = "github.com/0xJean/tabula/stdsql"
	pgxdriverPkgPath = "github.com/0xJean/tabula/pgxdriver"
)

// SQLMethod matches the package-qualified receiver type (without pointer or
// generic arguments). Argument is the zero-based SQL position in a method call.
type SQLMethod struct {
	Receiver, Method string
	Argument         int
}

// SQLRoutineMethod matches a method call that invokes a PostgreSQL routine by
// name and positional argument slice rather than raw SQL text. NameArgument and
// ArgsArgument are zero-based parameter positions after the method receiver.
// Schema defaults to "public". When SetReturning is true, static calls emit
// SELECT * FROM <schema>.<name>($1, ...); otherwise SELECT <schema>.<name>($1, ...).
type SQLRoutineMethod struct {
	Receiver     string
	Method       string
	NameArgument int
	ArgsArgument int
	Schema       string `json:",omitempty"`
	SetReturning bool   `json:",omitempty"`
}

// CallTarget matches a Go package-level function (Receiver == ""), method
// (Receiver != ""), or struct field (Field == true) by canonical package path
// and symbol name. Field targets also match selections promoted by embedding.
type CallTarget struct {
	Package  string
	Receiver string `json:",omitempty"`
	Name     string
	Field    bool   `json:",omitempty"`
	Category string `json:",omitempty"`
}

// CallSite records one resolved reference or invocation of a CallTarget.
// Escaped is true when a function or method is referenced as a value rather
// than called directly.
type CallSite struct {
	Package  string
	File     string
	Line     int
	Symbol   string
	Category string `json:",omitempty"`
	Escaped  bool   `json:",omitempty"`
}

// DefaultSecurityCallTargets returns the built-in security-review call targets:
// session keys, registry sealing, proof signing, keyed runner/Direct factories,
// nested-transaction marker helpers, and both drivers' raw transaction access.
func DefaultSecurityCallTargets() []CallTarget {
	return []CallTarget{
		{Package: tabulaPkgPath, Name: "NewSessionKey", Category: "session_key"},
		{Package: tabulaPkgPath, Receiver: tabulaPkgPath + ".Registry", Name: "Seal", Category: "registry_seal"},
		{Package: tabulaPkgPath, Receiver: tabulaPkgPath + ".SessionKey", Name: "Sign", Category: "session_sign"},
		{Package: tabulaPkgPath, Name: "WithoutTransaction", Category: "tx_escape"},
		{Package: tabulaPkgPath, Name: "MarkTransaction", Category: "tx_marker"},
		{Package: tabulaPkgPath, Name: "InTransaction", Category: "tx_marker"},
		{Package: stdsqlPkgPath, Name: "NewRunner", Category: "runner_factory"},
		{Package: stdsqlPkgPath, Name: "DirectSessionKey", Category: "direct_factory"},
		{Package: stdsqlPkgPath, Name: "NewSession", Category: "unsealed_session"},
		{Package: stdsqlPkgPath, Receiver: stdsqlPkgPath + ".Runner", Name: "WithinRaw", Category: "raw_tx"},
		{Package: stdsqlPkgPath, Receiver: stdsqlPkgPath + ".Runner", Name: "WithinRawOptions", Category: "raw_tx"},
		{Package: stdsqlPkgPath, Receiver: stdsqlPkgPath + ".RawSession", Name: "Tx", Category: "raw_tx"},
		{Package: stdsqlPkgPath, Receiver: stdsqlPkgPath + ".RunnerOptions", Name: "ExposeTx", Field: true, Category: "raw_tx"},
		{Package: pgxdriverPkgPath, Name: "NewRunner", Category: "runner_factory"},
		{Package: pgxdriverPkgPath, Name: "DirectWithConfig", Category: "direct_factory"},
		{Package: pgxdriverPkgPath, Name: "NewSession", Category: "unsealed_session"},
		{Package: pgxdriverPkgPath, Receiver: pgxdriverPkgPath + ".Runner", Name: "WithinRaw", Category: "raw_tx"},
		{Package: pgxdriverPkgPath, Receiver: pgxdriverPkgPath + ".Runner", Name: "WithinRawTxOptions", Category: "raw_tx"},
		{Package: pgxdriverPkgPath, Receiver: pgxdriverPkgPath + ".RawSession", Name: "Tx", Category: "raw_tx"},
		{Package: pgxdriverPkgPath, Receiver: pgxdriverPkgPath + ".RunnerOptions", Name: "ExposeTx", Field: true, Category: "raw_tx"},
	}
}

// InventoryConfig selects buildable Go packages, SQL methods, call targets,
// and reviewed site exceptions. Patterns default to ./... in Dir. Tags use the
// Go build tag syntax.
type InventoryConfig struct {
	Dir      string
	Patterns []string
	Tags     string
	// Tests includes internal and external _test.go files in matching packages.
	Tests          bool
	Methods        []SQLMethod
	RoutineMethods []SQLRoutineMethod `json:",omitempty"`
	Exceptions     map[string]string
	// CallTargets collects typed references to package-level functions, methods,
	// and struct fields for review (reported in SQLInventory.CallSites).
	CallTargets []CallTarget
	// AuditSecurityCalls adds DefaultSecurityCallTargets() to CallTargets.
	AuditSecurityCalls bool
}

// SQLSite describes a static or dynamic SQL call, composed query definition,
// or single-table aggregate call. ID is independent of line numbers.
type SQLSite struct {
	ID, File         string
	Line             int
	Receiver, Method string
	SQL              string `json:",omitempty"`
	QueryKey         string `json:",omitempty"`
	// Definition is the owning descriptor when its name is resolved from source.
	Definition string `json:",omitempty"`
	// NoFilter records a scalar aggregate call with an explicit nil where argument.
	// Only these calls can use the aggregate SQL from a descriptor snapshot.
	NoFilter  bool   `json:",omitempty"`
	Kind      string `json:",omitempty"`
	Dynamic   bool   `json:",omitempty"`
	Exception string `json:",omitempty"`
}

// SQLInventory retains reviewed sites, typed security/factory call sites, and
// reports unreviewed dynamic SQL and stale exceptions.
type SQLInventory struct {
	Sites     []SQLSite
	CallSites []CallSite `json:",omitempty"`
	Findings  []string
}

type buildPackage struct {
	ImportPath, Dir, Export string
	CompiledGoFiles         []string
	ForTest                 string
	DepOnly                 bool
	ImportMap               map[string]string
}

// CollectSQL resolves Go types and constants using the installed Go toolchain,
// including aliases and imported constants. It recognizes View.DefineQuery,
// View.TryDefineQuery, and single-table aggregate calls (Max, Min, Sum, Avg,
// CountBy, Exists) as static sites when their key or column is compile-time
// constant, and collects typed call sites for CallTargets / AuditSecurityCalls.
// Escaping function or method values are flagged accordingly.
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
	routines, err := compileRoutineMethods(methods, cfg.RoutineMethods)
	if err != nil {
		return out, err
	}
	allTargets := slices.Clone(cfg.CallTargets)
	if cfg.AuditSecurityCalls {
		allTargets = append(allTargets, DefaultSecurityCallTargets()...)
	}
	if len(methods) == 0 && len(routines) == 0 && len(allTargets) == 0 {
		return out, fmt.Errorf("specify at least one SQL method or call target")
	}
	targets, err := compileCallTargets(allTargets)
	if err != nil {
		return out, err
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
		sites, callSites, err := p.inventory(ctx, root, exports, methods, routines, targets)
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
		out.CallSites = append(out.CallSites, callSites...)
	}
	for id := range cfg.Exceptions {
		if !seen[id] {
			out.Findings = append(out.Findings, "stale SQL exception: "+id)
		}
	}
	slices.SortFunc(out.Sites, func(a, b SQLSite) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.CallSites, func(a, b CallSite) int {
		if c := strings.Compare(a.File, b.File); c != 0 {
			return c
		}
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return strings.Compare(a.Symbol, b.Symbol)
	})
	slices.Sort(out.Findings)
	return out, nil
}

type compiledCallTargets struct {
	funcs   map[[2]string]CallTarget
	methods map[[2]string]CallTarget
	fields  map[[2]string]CallTarget
}

func compileCallTargets(list []CallTarget) (compiledCallTargets, error) {
	ct := compiledCallTargets{
		funcs:   map[[2]string]CallTarget{},
		methods: map[[2]string]CallTarget{},
		fields:  map[[2]string]CallTarget{},
	}
	for _, t := range list {
		if t.Name == "" {
			return ct, fmt.Errorf("invalid call target %+v: Name is required", t)
		}
		switch {
		case t.Field:
			if t.Receiver == "" {
				return ct, fmt.Errorf("invalid field call target %+v: Receiver is required", t)
			}
			ct.fields[[2]string{t.Receiver, t.Name}] = t
		case t.Receiver != "":
			ct.methods[[2]string{t.Receiver, t.Name}] = t
		default:
			if t.Package == "" {
				return ct, fmt.Errorf("invalid function call target %+v: Package is required", t)
			}
			ct.funcs[[2]string{t.Package, t.Name}] = t
		}
	}
	return ct, nil
}

var tabulaAggregateFuncs = map[string]bool{
	"Max":     true,
	"Min":     true,
	"Sum":     true,
	"Avg":     true,
	"CountBy": true,
}

func unwrapCallTarget(fun ast.Expr) ast.Expr {
	for {
		fun = ast.Unparen(fun)
		switch x := fun.(type) {
		case *ast.IndexExpr:
			fun = x.X
		case *ast.IndexListExpr:
			fun = x.X
		default:
			return fun
		}
	}
}

func (p buildPackage) inventory(
	ctx context.Context,
	root string,
	exports map[string]string,
	methods map[[2]string]SQLMethod,
	routines map[[2]string]SQLRoutineMethod,
	targets compiledCallTargets,
) ([]SQLSite, []CallSite, error) {
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range p.CompiledGoFiles {
		if !filepath.IsAbs(name) {
			name = filepath.Join(p.Dir, name)
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, file)
	}
	info := &types.Info{
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
	}
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
	pkgPath, _, _ := strings.Cut(p.ImportPath, " [")
	if _, err := conf.Check(pkgPath, fset, files, info); err != nil {
		return nil, nil, err
	}

	var sites []SQLSite
	var callSites []CallSite
	counts := map[string]int{}
	addSite := func(owner string, site SQLSite, exprs ...ast.Expr) {
		var b bytes.Buffer
		for i, expr := range exprs {
			if i > 0 {
				b.WriteByte('\n')
			}
			_ = format.Node(&b, fset, expr)
		}
		identity := owner + "\n" + site.Receiver + "." + site.Method + "\n" + b.String() + "\n" + site.SQL + "\n" + site.QueryKey
		if site.Definition != "" {
			identity += "\n" + site.Definition
		}
		digest := sha256.Sum256([]byte(identity))
		base := fmt.Sprintf("%s#%x", site.File, digest[:12])
		counts[base]++
		site.ID = fmt.Sprintf("%s-%d", base, counts[base])
		sites = append(sites, site)
	}
	addField := func(recv, name string, at token.Pos) {
		if t, ok := targets.fields[[2]string{recv, name}]; ok {
			pos := fset.PositionFor(at, true)
			rel, _ := filepath.Rel(root, pos.Filename)
			callSites = append(callSites, CallSite{
				Package: pkgPath, File: filepath.ToSlash(rel), Line: pos.Line,
				Symbol: recv + "." + name, Category: t.Category,
			})
		}
	}

	source := inventorySource{info: info, initializers: singleInitializers(files, info)}
	for _, file := range files {
		if p.ForTest != "" && !strings.HasSuffix(fset.PositionFor(file.Pos(), false).Filename, "_test.go") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		selectorCalls := map[*ast.SelectorExpr]*ast.CallExpr{}
		identCalls := map[*ast.Ident]*ast.CallExpr{}
		ast.Inspect(file, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				switch fn := unwrapCallTarget(c.Fun).(type) {
				case *ast.SelectorExpr:
					selectorCalls[fn] = c
					identCalls[fn.Sel] = c
				case *ast.Ident:
					identCalls[fn] = c
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
				switch node := n.(type) {
				case *ast.SelectorExpr:
					sel := info.Selections[node]
					pos := fset.PositionFor(node.Pos(), true)
					rel, _ := filepath.Rel(root, pos.Filename)
					rel = filepath.ToSlash(rel)

					if sel != nil && sel.Kind() == types.FieldVal {
						recv := receiverName(sel.Recv())
						addField(recv, node.Sel.Name, node.Pos())
						if declaring := fieldReceiver(sel); declaring != recv {
							addField(declaring, node.Sel.Name, node.Pos())
						}
						return true
					}
					if sel == nil {
						return true
					}
					recvName, sigRecvName := methodReceiverNames(sel)
					// 1. Registered SQL methods.
					m, ok := methods[[2]string{recvName, node.Sel.Name}]
					if !ok && sigRecvName != "" {
						m, ok = methods[[2]string{sigRecvName, node.Sel.Name}]
					}
					if ok {
						site := SQLSite{File: rel, Line: pos.Line, Method: m.Method, Receiver: m.Receiver, Dynamic: true}
						var expr ast.Expr = node
						if c := selectorCalls[node]; c != nil {
							i := m.Argument
							if sel.Kind() == types.MethodExpr {
								i++
							}
							if i < len(c.Args) {
								expr = c.Args[i]
								if sVal, ok := source.constantString(expr); ok {
									site.SQL = sVal
									site.Dynamic = false
								}
							}
						}
						addSite(owner, site, expr)
					}

					// 1b. Registered SQL routine methods.
					rm, ok := routines[[2]string{recvName, node.Sel.Name}]
					if !ok && sigRecvName != "" {
						rm, ok = routines[[2]string{sigRecvName, node.Sel.Name}]
					}
					if ok {
						site := SQLSite{File: rel, Line: pos.Line, Method: rm.Method, Receiver: rm.Receiver, Dynamic: true}
						idExprs := []ast.Expr{node}
						if c := selectorCalls[node]; c != nil {
							offset := 0
							if sel.Kind() == types.MethodExpr {
								offset = 1
							}
							nameIdx, argsIdx := rm.NameArgument+offset, rm.ArgsArgument+offset
							if max(nameIdx, argsIdx) < len(c.Args) {
								idExprs = []ast.Expr{c.Args[nameIdx], c.Args[argsIdx]}
								name, nameOK := source.constantString(c.Args[nameIdx])
								arity, argsOK := source.literalSliceLen(c.Args[argsIdx])
								if nameOK && name != "" && !strings.ContainsRune(name, 0) && !c.Ellipsis.IsValid() && argsOK {
									site.SQL = routineSQL(rm.Schema, name, arity, rm.SetReturning)
									site.Dynamic = false
								}
							}
						}
						addSite(owner, site, idExprs...)
					}

					// 2. Built-in View.DefineQuery / View.TryDefineQuery static site recognition.
					if (recvName == tabulaPkgPath+".View" || sigRecvName == tabulaPkgPath+".View") &&
						(node.Sel.Name == "DefineQuery" || node.Sel.Name == "TryDefineQuery") {
						site := SQLSite{
							File:     rel,
							Line:     pos.Line,
							Receiver: tabulaPkgPath + ".View",
							Method:   node.Sel.Name,
							Kind:     "composed_query",
							Dynamic:  true,
						}
						var expr ast.Expr = node
						if c := selectorCalls[node]; c != nil {
							site.Definition = source.definitionName(methodReceiver(node, sel, c))
							keyIdx := 0
							if sel.Kind() == types.MethodExpr {
								keyIdx++
							}
							if keyIdx < len(c.Args) {
								expr = c.Args[keyIdx]
								if keyVal, ok := source.constantString(expr); ok && keyVal != "" {
									site.QueryKey = keyVal
									site.Dynamic = false
								}
							}
						}
						addSite(owner, site, expr)
					}

					// 3. Built-in Rows.Exists static aggregate recognition.
					if (recvName == tabulaPkgPath+".Rows" || sigRecvName == tabulaPkgPath+".Rows") &&
						node.Sel.Name == "Exists" {
						site := SQLSite{
							File:     rel,
							Line:     pos.Line,
							Receiver: tabulaPkgPath + ".Rows",
							Method:   "Exists",
							Kind:     "aggregate",
							Dynamic:  selectorCalls[node] == nil,
						}
						if !site.Dynamic {
							site.QueryKey = "exists"
							site.Definition = source.definitionName(methodReceiver(node, sel, selectorCalls[node]))
						}
						addSite(owner, site, node)
					}

					// 4. Method CallTargets.
					t, found := targets.methods[[2]string{recvName, node.Sel.Name}]
					matchRecv := recvName
					if !found && sigRecvName != "" {
						t, found = targets.methods[[2]string{sigRecvName, node.Sel.Name}]
						matchRecv = sigRecvName
					}
					if found {
						callSites = append(callSites, CallSite{
							Package:  pkgPath,
							File:     rel,
							Line:     pos.Line,
							Symbol:   matchRecv + "." + node.Sel.Name,
							Category: t.Category,
							Escaped:  selectorCalls[node] == nil,
						})
					}

				case *ast.CompositeLit:
					recv := receiverName(info.TypeOf(node))
					for _, elt := range node.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if id, ok := kv.Key.(*ast.Ident); ok {
								if field, ok := info.Uses[id].(*types.Var); ok && field.IsField() {
									addField(recv, id.Name, id.Pos())
								}
							}
						}
					}

				case *ast.Ident:
					fn, ok := info.Uses[node].(*types.Func)
					if !ok || fn.Pkg() == nil {
						return true
					}
					sig, ok := fn.Type().(*types.Signature)
					if !ok || sig.Recv() != nil {
						return true
					}
					fnPkg := fn.Pkg().Path()
					fnName := fn.Name()
					pos := fset.PositionFor(node.Pos(), true)
					rel, _ := filepath.Rel(root, pos.Filename)
					rel = filepath.ToSlash(rel)

					// Built-in single-table package-level aggregates: tabula.{Max,Min,Sum,Avg,CountBy}.
					if fnPkg == tabulaPkgPath && tabulaAggregateFuncs[fnName] {
						site := SQLSite{
							File:     rel,
							Line:     pos.Line,
							Receiver: tabulaPkgPath,
							Method:   fnName,
							Kind:     "aggregate",
							Dynamic:  true,
						}
						var expr ast.Expr = node
						if c := identCalls[node]; c != nil && len(c.Args) > 2 {
							expr = c.Args[2]
							site.Definition = source.definitionName(c.Args[1])
							if fnName != "CountBy" && len(c.Args) == 4 {
								arg, ok := ast.Unparen(c.Args[3]).(*ast.Ident)
								site.NoFilter = ok && info.Uses[arg] == types.Universe.Lookup("nil")
							}
							if col, ok := source.constantString(expr); ok && col != "" {
								site.QueryKey = strings.ToLower(fnName) + ":" + col
								site.Dynamic = false
							}
						}
						addSite(owner, site, expr)
					}

					// Package-level function CallTargets (works with import aliases, dot imports, generics, escaped values).
					if t, found := targets.funcs[[2]string{fnPkg, fnName}]; found {
						callSites = append(callSites, CallSite{
							Package:  pkgPath,
							File:     rel,
							Line:     pos.Line,
							Symbol:   fnPkg + "." + fnName,
							Category: t.Category,
							Escaped:  identCalls[node] == nil,
						})
					}
				}
				return true
			})
		}
	}
	return sites, callSites, nil
}

func methodReceiverNames(sel *types.Selection) (string, string) {
	recv := receiverName(sel.Recv())
	var sigRecv string
	if sig, ok := sel.Obj().Type().(*types.Signature); ok && sig.Recv() != nil {
		sigRecv = receiverName(sig.Recv().Type())
	}
	return recv, sigRecv
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
