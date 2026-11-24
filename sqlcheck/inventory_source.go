package sqlcheck

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
)

type inventorySource struct {
	info         *types.Info
	initializers map[types.Object]ast.Expr
}

func (s inventorySource) constantString(expr ast.Expr) (string, bool) {
	value := s.info.Types[expr].Value
	if id, ok := ast.Unparen(expr).(*ast.Ident); ok && value == nil {
		if v, ok := s.info.Uses[id].(*types.Var); ok && v.Pkg() != nil && v.Parent() != v.Pkg().Scope() {
			value = s.info.Types[s.initializers[v]].Value
		}
	}
	if value == nil || value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(value), true
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
