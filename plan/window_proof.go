package plan

import (
	"fmt"

	"github.com/0xJean/tabula"
)

// Window results depend on the rows admitted to their partition, even when
// the cursor only orders by a physical key. A seek must therefore be applied
// to the exported result rather than the window's input rows.
func readWindowProof(q ReadSelect, sources []rowSource, order []RowTerm) ([]tabula.KeyRequirement, []tabula.ComparisonRequirement, error) {
	var keys []tabula.KeyRequirement
	var comparisons []tabula.ComparisonRequirement
	var visit func(expressionPart) error
	visit = func(p expressionPart) error {
		t, ok := expressionStructure(p)
		if !ok {
			return fmt.Errorf("tabula: unknown window result expression")
		}
		if n, ok := p.(windowExpression); ok {
			if a, ok := n.function.(aggregateCall); ok {
				// A deterministic window ORDER BY supplies the input order of
				// order-sensitive aggregates. Their representation and floating
				// accumulation restrictions still apply.
				a.order = n.spec.Order
				if err := repeatableReadAggregate(a); err != nil {
					return err
				}
			}
			terms := make([]RowTerm, 0, len(n.spec.Partition)+len(n.spec.Order))
			for _, part := range scalarArgs(n.spec.Partition...) {
				terms = append(terms, RowTerm{expr: part})
			}
			terms = append(terms, n.spec.Order...)
			base := ReadSelect{group: q.group}
			var required []tabula.KeyRequirement
			var compared []tabula.ComparisonRequirement
			var err error
			if len(q.group) == 0 && readHasAggregate(q) {
				compared, err = readOrderComparisons(terms)
			} else {
				required, compared, err = readResultProof(base, sources, terms)
			}
			if err != nil {
				return fmt.Errorf("tabula: window cursor needs deterministic partition ordering: %w", err)
			}
			keys = appendKeyRequirements(keys, required...)
			comparisons = appendComparisonRequirements(comparisons, compared...)
		}
		for _, child := range t.children {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	for _, term := range order {
		if err := visit(term.orderValue().part); err != nil {
			return nil, nil, err
		}
	}
	return keys, comparisons, nil
}
