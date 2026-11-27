package sqlcheck

import (
	"go/ast"
	"go/types"
	"strings"
)

func rowDeclaration(name string) bool {
	switch strings.TrimPrefix(name, "Try") {
	case "DefineRows", "DefineRowsWithForms", "DefineTableRows", "DefineExists", "DefineRead", "DefineTableRead", "DefineUpdateMany", "DefineDeleteMany":
		return true
	}
	return false
}

func rowExecutionMethod(recv, method string) bool {
	switch recv {
	case tabulaPkgPath + ".BoundRowsPlan":
		return method == "Count" || method == "Exists" || method == "Find" || method == "List" || singleRowMethod(method)
	case tabulaPkgPath + ".BoundReadPlan":
		return method == "Find" || method == "Page" || method == "List" || method == "Count" || singleRowMethod(method)
	case tabulaPkgPath + ".BoundExistsPlan":
		return method == "Exists"
	case tabulaPkgPath + ".BoundMutationPlan":
		return method == "UpdateMany" || method == "DeleteMany"
	}
	return false
}

func singleRowMethod(method string) bool {
	return method == "One" || method == "MaybeOne" || method == "First" || method == "MaybeFirst"
}

func tabulaCallName(info *types.Info, call *ast.CallExpr) string {
	var id *ast.Ident
	switch f := unwrapCallTarget(call.Fun).(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	}
	fn, ok := info.Uses[id].(*types.Func)
	if ok && fn.Pkg() != nil && fn.Pkg().Path() == tabulaPkgPath {
		return fn.Name()
	}
	return ""
}

func isTryPlanHandle(info *types.Info, call *ast.CallExpr) bool {
	name := tabulaCallName(info, call)
	return name == "TryMapTable" || name == "TryDefine" || name == "TryDefinePlanSource" ||
		strings.HasPrefix(name, "Try") && typedPlanSiteKind(tabulaPkgPath, name) != ""
}

func (s inventorySource) rowPlan(expr ast.Expr, depth int) (definition, key string) {
	if depth >= 64 {
		return "", ""
	}
	switch e := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return s.rowPlan(s.initializers[s.info.Uses[e]], depth+1)
	case *ast.CallExpr:
		name := tabulaCallName(s.info, e)
		ownerArg, keyArg := planOwnerArguments(name)
		if typedPlanSiteKind(tabulaPkgPath, name) != "" && len(e.Args) > keyArg {
			key, _ = s.constantString(e.Args[keyArg])
			return s.definitionName(e.Args[ownerArg]), key
		}
		if selector, ok := unwrapCallTarget(e.Fun).(*ast.SelectorExpr); ok && selector.Sel.Name == "Bind" {
			if sel := s.info.Selections[selector]; sel != nil {
				_, recv := methodReceiverNames(sel)
				switch recv {
				case tabulaPkgPath + ".RowsPlan", tabulaPkgPath + ".ReadPlan", tabulaPkgPath + ".ExistsPlan", tabulaPkgPath + ".MutationPlan", tabulaPkgPath + ".RoutinePlan", tabulaPkgPath + ".AdvisoryLockPlan":
					return s.rowPlan(methodReceiver(selector, sel, e), depth+1)
				case tabulaPkgPath + ".WritePlan", tabulaPkgPath + ".ReturningPlan", tabulaPkgPath + ".MutationReturningPlan":
					return s.rowPlan(methodReceiver(selector, sel, e), depth+1)
				}
			}
		}
	}
	return "", ""
}
