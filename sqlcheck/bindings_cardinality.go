package sqlcheck

import (
	"strings"

	"github.com/0xJean/tabula"
)

func boundedRowForm(form string) bool {
	return form == "single" || form == "firstrow" || form == "single_first"
}

// Match the shape that the bound method can actually execute. A bounded
// exact declaration takes precedence over its unpaged data shape, including
// when the binding's prefix would otherwise select only that data shape.
func matchesSingleRow(key, plan, method string, shape tabula.StatementShape, d tabula.Definition) bool {
	rest, ok := strings.CutPrefix(key, plan+".")
	if !ok {
		return false
	}
	scope, path, _ := strings.Cut(rest, ".")
	form, tail, _ := strings.Cut(path, ".")
	if method == "First" || method == "MaybeFirst" {
		return form == "firstrow" || form == "single_first"
	}
	if boundedRowForm(form) {
		return form != "firstrow"
	}
	if form != "data" || shape.Effects&tabula.EffectRowLock != 0 {
		return false
	}
	// ORDER names may themselves be "offset", "where" or "first".
	remaining := tail
	if order, sorted := strings.CutPrefix(tail, "sort."); sorted {
		_, remaining, _ = strings.Cut(order, ".")
	}
	if remaining != "" && !strings.HasPrefix(remaining, "where.") {
		return false
	}
	prefix := plan + "." + scope + "."
	suffix := strings.TrimPrefix(path, form)
	return d.StatementQueries[prefix+"single"+suffix] != plan && d.StatementQueries[prefix+"single_first"+suffix] != plan
}
