package sqlcheck

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strconv"
	"strings"
)

type inventorySource struct {
	info         *types.Info
	initializers map[types.Object]ast.Expr
}

func (s inventorySource) constantString(expr ast.Expr) (string, bool) {
	return s.evalString(expr, 0)
}

func (s inventorySource) evalString(expr ast.Expr, depth int) (string, bool) {
	if expr == nil || depth > 32 {
		return "", false
	}
	expr = ast.Unparen(expr)
	if value := s.info.Types[expr].Value; value != nil && value.Kind() == constant.String {
		return constant.StringVal(value), true
	}
	switch x := expr.(type) {
	case *ast.Ident:
		if v, ok := s.info.Uses[x].(*types.Var); ok && v.Pkg() != nil && v.Parent() != v.Pkg().Scope() {
			return s.evalString(s.initializers[v], depth+1)
		}
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			left, ok := s.evalString(x.X, depth+1)
			if !ok {
				return "", false
			}
			right, ok := s.evalString(x.Y, depth+1)
			if !ok {
				return "", false
			}
			return left + right, true
		}
	}
	return "", false
}

func (s inventorySource) literalSliceLen(expr ast.Expr) (int, bool) {
	expr = ast.Unparen(expr)
	if id, ok := expr.(*ast.Ident); ok && s.info.Uses[id] == types.Universe.Lookup("nil") {
		return 0, true
	}
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return 0, false
	}
	t := s.info.TypeOf(lit)
	if t == nil {
		return 0, false
	}
	if _, isSlice := t.Underlying().(*types.Slice); !isSlice {
		return 0, false
	}
	for _, elt := range lit.Elts {
		if _, keyed := elt.(*ast.KeyValueExpr); keyed {
			return 0, false
		}
	}
	return len(lit.Elts), true
}

func compileRoutineMethods(methods map[[2]string]SQLMethod, list []SQLRoutineMethod) (map[[2]string]SQLRoutineMethod, error) {
	out := make(map[[2]string]SQLRoutineMethod, len(list))
	for _, m := range list {
		if m.Schema == "" {
			m.Schema = "public"
		}
		if m.Receiver == "" || m.Method == "" || m.NameArgument < 0 || m.ArgsArgument < 0 || m.NameArgument == m.ArgsArgument || strings.ContainsRune(m.Schema, 0) {
			return nil, fmt.Errorf("invalid SQL routine method %+v", m)
		}
		k := [2]string{m.Receiver, m.Method}
		if _, dup := methods[k]; dup {
			return nil, fmt.Errorf("duplicate SQL method %v", k)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("duplicate SQL routine method %v", k)
		}
		out[k] = m
	}
	return out, nil
}

func routineSQL(schema, name string, arity int, setReturning bool) string {
	holders := make([]string, arity)
	for i := range holders {
		holders[i] = "$" + strconv.Itoa(i+1)
	}
	call := quoteIdent(schema) + "." + quoteIdent(name) + "(" + strings.Join(holders, ", ") + ")"
	if setReturning {
		return "SELECT * FROM " + call
	}
	return "SELECT " + call
}

// Follow direct declarations and Table.Bind through single-assignment handles.
// Require an explicit binding for factory results, parameters and mutable handles.
func (s inventorySource) definitionName(expr ast.Expr) string {
	switch e := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return s.definitionName(s.initializers[s.info.Uses[e]])
	case *ast.CallExpr:
		var id *ast.Ident
		switch callee := unwrapCallTarget(e.Fun).(type) {
		case *ast.Ident:
			id = callee
		case *ast.SelectorExpr:
			if sel := s.info.Selections[callee]; sel != nil {
				_, declared := methodReceiverNames(sel)
				if declared == tabulaPkgPath+".Table" && callee.Sel.Name == "Bind" {
					return s.definitionName(methodReceiver(callee, sel, e))
				}
				return ""
			}
			id = callee.Sel
		}
		fn, ok := s.info.Uses[id].(*types.Func)
		if !ok || fn.Pkg() == nil || fn.Pkg().Path() != tabulaPkgPath ||
			(fn.Name() != "Define" && fn.Name() != "DefineView") || len(e.Args) != 2 {
			return ""
		}
		// A spec stored in a variable can have its fields mutated independently.
		if spec, ok := ast.Unparen(e.Args[1]).(*ast.CompositeLit); ok {
			for _, elt := range spec.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if field, ok := kv.Key.(*ast.Ident); ok && field.Name == "Name" {
						name, _ := s.constantString(kv.Value)
						return name
					}
				}
			}
		}
	}
	return ""
}

func methodReceiver(node *ast.SelectorExpr, sel *types.Selection, call *ast.CallExpr) ast.Expr {
	if sel.Kind() == types.MethodExpr && len(call.Args) > 0 {
		return call.Args[0]
	}
	return node.X
}

// A field selection's index path includes each embedded field and ends at
// the selected field in its declaring struct.
func fieldReceiver(sel *types.Selection) string {
	t := sel.Recv()
	path := sel.Index()
	for i, index := range path {
		if p, ok := t.Underlying().(*types.Pointer); ok {
			t = p.Elem()
		}
		if i == len(path)-1 {
			return receiverName(t)
		}
		st, ok := t.Underlying().(*types.Struct)
		if !ok {
			return ""
		}
		t = st.Field(index).Type()
	}
	return ""
}

// Inspect the whole package for writes to private package variables as well
// as locals. An exported package variable can also be assigned by a caller.
func singleInitializers(files []*ast.File, info *types.Info) map[types.Object]ast.Expr {
	out := map[types.Object]ast.Expr{}
	record := func(e ast.Expr, v ast.Expr) {
		if ptr, ok := ast.Unparen(e).(*ast.StarExpr); ok {
			e, v = ptr.X, nil
		}
		id, ok := ast.Unparen(e).(*ast.Ident)
		if !ok {
			return
		}
		obj := info.ObjectOf(id)
		if obj == nil || obj.Pkg() == nil || (obj.Exported() && obj.Parent() == obj.Pkg().Scope()) {
			return
		}
		if _, seen := out[obj]; seen {
			v = nil
		}
		out[obj] = v
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for i, e := range x.Lhs {
					var v ast.Expr
					if x.Tok == token.DEFINE && len(x.Rhs) == len(x.Lhs) {
						v = x.Rhs[i]
					}
					record(e, v)
				}
			case *ast.ValueSpec:
				for i, id := range x.Names {
					var v ast.Expr
					if len(x.Values) == len(x.Names) {
						v = x.Values[i]
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
	}
	return out
}
