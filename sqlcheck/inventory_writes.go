package sqlcheck

import (
	"strconv"
	"strings"

	"github.com/0xJean/tabula"
)

// Only final registrations and their bound methods are execution roots. Query
// builders and intermediate common tables acquire the final owner's inventory.
func writePlanReceiver(receiver, method string) string {
	switch receiver {
	case planPkgPath:
		switch strings.TrimPrefix(method, "Try") {
		case "DefineWrite":
			return planPkgPath + ".BoundWritePlan"
		case "DefineReturning", "DefineTableReturning":
			return planPkgPath + ".BoundReturningPlan"
		case "DefineUpdateReturning", "DefineDeleteReturning":
			return planPkgPath + ".BoundMutationReturningPlan"
		}
	case planPkgPath + ".BoundWritePlan":
		if method == "Exec" || method == "ExecBatch" {
			return receiver
		}
	case planPkgPath + ".BoundReturningPlan":
		if method == "All" || method == "AllBatch" || method == "Iter" {
			return receiver
		}
	case planPkgPath + ".BoundMutationReturningPlan":
		if method == "UpdateMany" || method == "UpdateIter" || method == "DeleteMany" || method == "DeleteIter" {
			return receiver
		}
	}
	return ""
}

func planOwnerArguments(method string) (owner, key int) {
	switch strings.TrimPrefix(method, "Try") {
	case "DefineUpdateReturning", "DefineDeleteReturning":
		return 1, 2
	default:
		return 0, 1
	}
}

func (site SQLSite) matchesWriteShape(key, plan string, shape tabula.StatementShape) bool {
	if shape.Effects&tabula.EffectWrite == 0 {
		return false
	}
	tail, ok := strings.CutPrefix(key, plan+".")
	if !ok {
		return false
	}
	scope, tail, ok := strings.Cut(tail, ".")
	if !ok || scope != "global" && scope != "rls" && scope != "scoped" && scope != "unscoped" {
		return false
	}
	form, rest, _ := strings.Cut(tail, ".")
	fixed := tail == "write"
	batch := false
	if form == "rows" {
		size, err := strconv.Atoi(rest)
		batch = err == nil && size > 0 && strconv.Itoa(size) == rest
	}
	returns := len(shape.Result) > 0
	switch strings.TrimPrefix(site.Method, "Try") {
	case "DefineWrite":
		return !returns && (fixed || batch)
	case "DefineReturning", "DefineTableReturning":
		return returns && (fixed || batch)
	case "Exec":
		return !returns && fixed
	case "ExecBatch":
		return !returns && batch
	case "All", "Iter":
		return returns && fixed
	case "AllBatch":
		return returns && batch
	case "DefineUpdateReturning", "UpdateMany", "UpdateIter":
		set, filters, hasFilters := strings.Cut(rest, ".")
		return returns && form == "set" && inventoryShapeLabel(set) &&
			(!hasFilters || filters != "") && writeShapeFilters(filters)
	case "DefineDeleteReturning", "DeleteMany", "DeleteIter":
		return returns && form == "delete" && (tail == "delete" || rest != "") && writeShapeFilters(rest)
	}
	return false
}

func writeShapeFilters(tail string) bool {
	if tail == "" {
		return true
	}
	names, ok := strings.CutPrefix(tail, "where.")
	if !ok {
		return false
	}
	seen := make(map[string]bool)
	for name := range strings.SplitSeq(names, "+") {
		if !inventoryShapeLabel(name) || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}

func inventoryShapeLabel(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			continue
		}
		return false
	}
	return true
}
