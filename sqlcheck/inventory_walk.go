package sqlcheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

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
	methods map[[2]string]registeredSQLMethod,
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
						site, exprs := source.methodSite(m, node, sel, selectorCalls[node])
						site.File, site.Line = rel, pos.Line
						addSite(owner, site, exprs...)
					}

					if kind := typedPlanSiteKind(sigRecvName, node.Sel.Name); kind != "" {
						call := selectorCalls[node]
						site := SQLSite{File: rel, Line: pos.Line, Receiver: sigRecvName, Method: node.Sel.Name, Kind: kind}
						if call != nil {
							site.Definition, site.QueryKey = source.rowPlan(methodReceiver(node, sel, call), 0)
						} else if sel.Kind() == types.MethodVal {
							site.Definition, site.QueryKey = source.rowPlan(node.X, 0)
						}
						addSite(owner, site, node)
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

					if kind := typedPlanSiteKind(fnPkg, fnName); kind != "" {
						site := SQLSite{File: rel, Line: pos.Line, Receiver: fnPkg, Method: fnName, Kind: kind}
						var expr ast.Expr = node
						if call := identCalls[node]; call != nil {
							site.Definition, site.QueryKey = source.rowPlan(call, 0)
							expr = call
							if _, keyArg := planOwnerArguments(fnName); len(call.Args) > keyArg {
								expr = call.Args[keyArg]
							}
						}
						addSite(owner, site, expr)
					}

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
