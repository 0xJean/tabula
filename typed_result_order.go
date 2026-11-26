package tabula

import "slices"

type projectedOrderValue struct{ alias, name string }

func (v projectedOrderValue) render(*typedCompiler) (string, error) {
	return typedQuote(v.alias) + "." + typedQuote(v.name), nil
}
func (v projectedOrderValue) expressionTraits() expressionTraits {
	return expressionTraits{identity: "result/" + v.alias + "/" + v.name}
}
func (v projectedOrderValue) nullableAfterJoin(map[*occurrence]bool) bool { return false }

func projectedRowOrder(o *rowOrdering, project []Projection, alias string) *rowOrdering {
	result := *o
	result.terms = slices.Clone(o.terms)
	for i := range result.terms {
		term := &result.terms[i]
		value := term.orderValue()
		for _, projection := range project {
			if sameExpression(projection.part, value.part) {
				term.field = nil
				term.expr = scalarOperand{projectedOrderValue{alias, projection.field.Name}, value.kind, o.termNullable(i)}
				break
			}
		}
	}
	return &result
}

// Original field-only group seeks are equivalent below GROUP BY and retain
// their snapshot. Computed or aggregate order values use the result boundary.
func complexRowOrder(o *rowOrdering, groups []RowColumn) bool {
	for _, group := range groups {
		if group.ref == nil {
			return true
		}
	}
	return slices.ContainsFunc(o.terms, func(t RowTerm) bool { return t.field == nil })
}
