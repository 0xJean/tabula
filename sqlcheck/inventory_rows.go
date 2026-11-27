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
	case planPkgPath + ".BoundRowsPlan":
		return method == "Count" || method == "Exists" || method == "Find" || method == "List" || singleRowMethod(method)
	case planPkgPath + ".BoundReadPlan":
		return method == "Find" || method == "Page" || method == "List" || method == "Count" || singleRowMethod(method)
	case planPkgPath + ".BoundExistsPlan":
		return method == "Exists"
	case planPkgPath + ".BoundMutationPlan":
		return method == "UpdateMany" || method == "DeleteMany"
	}
	return false
}

func singleRowMethod(method string) bool {
	return method == "One" || method == "MaybeOne" || method == "First" || method == "MaybeFirst"
}

func pkgCallName(info *types.Info, call *ast.CallExpr) (pkg, name string) {
	var id *ast.Ident
	switch f := unwrapCallTarget(call.Fun).(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	}
	fn, ok := info.Uses[id].(*types.Func)
	if ok && fn.Pkg() != nil {
		return fn.Pkg().Path(), fn.Name()
	}
	return "", ""
}

func isTryPlanHandle(info *types.Info, call *ast.CallExpr) bool {
	pkg, name := pkgCallName(info, call)
	switch pkg {
	case tabulaPkgPath:
		return name == "TryDefine" || name == "TryDefinePlanSource"
	case planPkgPath:
		return name == "TryMapTable" || strings.HasPrefix(name, "Try") && typedPlanSiteKind(planPkgPath, name) != ""
	default:
		return false
	}
}

func (s inventorySource) rowPlan(expr ast.Expr, depth int) (definition, key string) {
	if depth >= 64 {
		return "", ""
	}
	switch e := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return s.rowPlan(s.initializers[s.info.Uses[e]], depth+1)
	case *ast.CallExpr:
		pkg, name := pkgCallName(s.info, e)
		ownerArg, keyArg := planOwnerArguments(name)
		if pkg == planPkgPath && typedPlanSiteKind(planPkgPath, name) != "" && len(e.Args) > keyArg {
			key, _ = s.constantString(e.Args[keyArg])
			return s.definitionName(e.Args[ownerArg]), key
		}
		if selector, ok := unwrapCallTarget(e.Fun).(*ast.SelectorExpr); ok && selector.Sel.Name == "Bind" {
			if sel := s.info.Selections[selector]; sel != nil {
				_, recv := methodReceiverNames(sel)
				switch recv {
				case planPkgPath + ".RowsPlan", planPkgPath + ".ReadPlan", planPkgPath + ".ExistsPlan", planPkgPath + ".MutationPlan", planPkgPath + ".RoutinePlan", planPkgPath + ".AdvisoryLockPlan":
					return s.rowPlan(methodReceiver(selector, sel, e), depth+1)
				case planPkgPath + ".WritePlan", planPkgPath + ".ReturningPlan", planPkgPath + ".MutationReturningPlan":
					return s.rowPlan(methodReceiver(selector, sel, e), depth+1)
				}
			}
		}
	}
	return "", ""
}
