package sqlcheck

import "strings"

// An explicit read keeps its original data and bounded keys. Only the list
// namespace admits a page call or a count that belongs to that page family.
func matchesReadList(key, plan, method string) bool {
	path, ok := strings.CutPrefix(key, plan+".")
	if !ok {
		return false
	}
	_, path, _ = strings.Cut(path, ".")
	path, ok = strings.CutPrefix(path, "list.")
	if !ok {
		return false
	}
	form, _, _ := strings.Cut(path, ".")
	switch method {
	case "Page":
		return form == "data"
	case "Count":
		return form == "count"
	case "List":
		return form == "data" || form == "count"
	default:
		return false
	}
}
