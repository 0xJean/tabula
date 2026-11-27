package sqlcheck

import (
	"strings"

	"github.com/0xJean/tabula"
)

// Source receiver types distinguish function and advisory operations, including
// method values that escape the scope of their bound session.
func effectPlanReceiver(receiver, method string) string {
	switch receiver {
	case tabulaPkgPath:
		switch strings.TrimPrefix(method, "Try") {
		case "DefineAdvisoryLock":
			return tabulaPkgPath + ".BoundAdvisoryLockPlan"
		case "DefineRoutine":
			return tabulaPkgPath + ".BoundRoutinePlan"
		}
	case tabulaPkgPath + ".BoundAdvisoryLockPlan":
		if method == "Acquire" {
			return receiver
		}
	case tabulaPkgPath + ".BoundRoutinePlan":
		if method == "One" || method == "All" {
			return receiver
		}
	}
	return ""
}

func typedPlanSiteKind(receiver, method string) string {
	if effectPlanReceiver(receiver, method) != "" {
		return "typed_plan"
	}
	if receiver == tabulaPkgPath && rowDeclaration(method) || rowExecutionMethod(receiver, method) || writePlanReceiver(receiver, method) != "" {
		return "typed_rows"
	}
	return ""
}

func (site SQLSite) matchesTypedShape(key, plan string, shape tabula.StatementShape, d tabula.Definition) bool {
	if site.Kind == "typed_rows" {
		if writePlanReceiver(site.Receiver, site.Method) != "" {
			return site.matchesWriteShape(key, plan, shape)
		}
		if singleRowMethod(site.Method) {
			return matchesSingleRow(key, plan, site.Method, shape, d)
		}
		if site.Receiver == tabulaPkgPath+".BoundReadPlan" {
			switch site.Method {
			case "Page", "List", "Count":
				return matchesReadList(key, plan, site.Method)
			}
		}
		return matchesRowMethod(key, plan, site.Method)
	}
	switch effectPlanReceiver(site.Receiver, site.Method) {
	case tabulaPkgPath + ".BoundAdvisoryLockPlan":
		return key == plan+".lock" && shape.Effects == tabula.EffectRead|tabula.EffectAdvisoryLock &&
			len(shape.Routines) == 0 && len(shape.Access) == 0
	case tabulaPkgPath + ".BoundRoutinePlan":
		if key != plan+".call" || len(shape.Routines) != 1 {
			return false
		}
		r := shape.Routines[0]
		effects := tabula.EffectWrite
		if r.ReadOnly {
			effects = tabula.EffectRead
		}
		return shape.Effects == effects && (site.Method != "One" || !r.ReturnsSet) && (site.Method != "All" || r.ReturnsSet)
	}
	return false
}
