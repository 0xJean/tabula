package sqlcheck

import (
	"go/ast"
	"go/types"
	"strings"
)

func rowDeclaration(name string) bool {
	switch name {
	case "DefineRows", "TryDefineRows", "DefineRead", "TryDefineRead", "DefineUpdateMany", "TryDefineUpdateMany", "DefineDeleteMany", "TryDefineDeleteMany":
		return true
	}
	return false
}

func rowExecutionMethod(recv, method string) bool {
	switch recv {
	case tabulaPkgPath + ".BoundRowsPlan":
		return method == "Count" || method == "Exists" || method == "Find" || method == "List"
	case tabulaPkgPath + ".BoundReadPlan":
		return method == "Find"
	case tabulaPkgPath + ".BoundMutationPlan":
		return method == "UpdateMany" || method == "DeleteMany"
	}
	return false
}

func rowFunction(info *types.Info, call *ast.CallExpr) string {
	var id *ast.Ident
	switch f := unwrapCallTarget(call.Fun).(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	}
	fn, ok := info.Uses[id].(*types.Func)
	if ok && fn.Pkg() != nil && fn.Pkg().Path() == tabulaPkgPath && typedPlanSiteKind(tabulaPkgPath, fn.Name()) != "" {
		return fn.Name()
	}
	return ""
}

func isTryRowPlan(info *types.Info, call *ast.CallExpr) bool {
	return strings.HasPrefix(rowFunction(info, call), "Try")
}

func (s inventorySource) rowPlan(expr ast.Expr, depth int) (definition, key string) {
	if depth >= 64 {
		return "", ""
	}
	switch e := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return s.rowPlan(s.initializers[s.info.Uses[e]], depth+1)
	case *ast.CallExpr:
		if rowFunction(s.info, e) != "" && len(e.Args) >= 2 {
			key, _ = s.constantString(e.Args[1])
			return s.definitionName(e.Args[0]), key
		}
		if selector, ok := unwrapCallTarget(e.Fun).(*ast.SelectorExpr); ok && selector.Sel.Name == "Bind" {
			if sel := s.info.Selections[selector]; sel != nil {
				_, recv := methodReceiverNames(sel)
				switch recv {
				case tabulaPkgPath + ".RowsPlan", tabulaPkgPath + ".ReadPlan", tabulaPkgPath + ".MutationPlan", tabulaPkgPath + ".RoutinePlan", tabulaPkgPath + ".AdvisoryLockPlan":
					return s.rowPlan(methodReceiver(selector, sel, e), depth+1)
				}
			}
		}
	}
	return "", ""
}
