package plan

import (
	"fmt"
	"slices"
	"strings"
)

func (n windowExpression) validate(outer map[*occurrence]bool) error {
	switch f := n.function.(type) {
	case windowFunction:
	case aggregateCall:
		if err := f.validatePhase(outer, true); err != nil {
			return err
		}
	default:
		return fmt.Errorf("tabula: Over requires a window function or ordinary aggregate")
	}
	if err := checkResultExpr(n, func(expressionPart) error { return nil }); err != nil {
		return err
	}
	for _, part := range scalarArgs(n.spec.Partition...) {
		if part.part == nil || !groupOrderComparable(part) {
			return fmt.Errorf("tabula: window partition requires a comparable expression")
		}
	}
	for _, term := range n.spec.Order {
		if !groupOrderComparable(term.orderValue()) {
			return fmt.Errorf("tabula: window ordering requires a comparable expression")
		}
	}
	return n.spec.Frame.validate(n.spec.Order)
}

func (spec WindowSpec) render(c *typedCompiler) (string, error) {
	var clauses []string
	if len(spec.Partition) > 0 {
		parts := make([]string, len(spec.Partition))
		for i, arg := range scalarArgs(spec.Partition...) {
			if arg.part == nil {
				return "", fmt.Errorf("tabula: undeclared window partition")
			}
			var err error
			parts[i], err = arg.part.render(c)
			if err != nil {
				return "", err
			}
		}
		clauses = append(clauses, "PARTITION BY "+strings.Join(parts, ", "))
	}
	if len(spec.Order) > 0 {
		order, err := (&rowOrdering{terms: spec.Order}).orderSQL(c)
		if err != nil {
			return "", err
		}
		clauses = append(clauses, strings.TrimPrefix(order, " "))
	}
	frame, err := spec.Frame.render(c, spec.Order)
	if err != nil {
		return "", err
	}
	if frame != "" {
		clauses = append(clauses, frame)
	}
	return strings.Join(clauses, " "), nil
}

func hasWindow(p expressionPart) bool {
	t, ok := expressionStructure(p)
	return ok && (t.window || slices.ContainsFunc(t.children, hasWindow))
}

func (q ReadSelect) hasWindows() bool {
	return slices.ContainsFunc(q.project, func(p Projection) bool { return hasWindow(p.part) }) ||
		slices.ContainsFunc(q.order, func(term RowTerm) bool { return hasWindow(term.orderValue().part) })
}

func (q ReadSelect) windowClause(c *typedCompiler) (string, error) {
	var ordered []WindowSpec
	seen := make(map[string]windowExpression)
	var visit func(expressionPart) error
	visit = func(p expressionPart) error {
		traits, ok := expressionStructure(p)
		if !ok {
			return fmt.Errorf("tabula: undeclared window expression")
		}
		if n, ok := p.(windowExpression); ok && n.spec.Name != "" {
			if !typedIdent(n.spec.Name) {
				return fmt.Errorf("tabula: invalid window name")
			}
			// Compare specification grammar/identities independently of the
			// function, so several different functions may share one window.
			n.function = RowNumber().part
			if prior, exists := seen[n.spec.Name]; exists {
				if !sameExpression(prior, n) {
					return fmt.Errorf("tabula: conflicting named window %q", n.spec.Name)
				}
			} else {
				seen[n.spec.Name] = n
				ordered = append(ordered, n.spec)
			}
		}
		for _, child := range traits.children {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	for _, project := range q.project {
		if err := visit(project.part); err != nil {
			return "", err
		}
	}
	for _, term := range q.order {
		if err := visit(term.orderValue().part); err != nil {
			return "", err
		}
	}
	if len(ordered) == 0 {
		return "", nil
	}
	parts := make([]string, len(ordered))
	for i, spec := range ordered {
		body, err := spec.render(c)
		if err != nil {
			return "", err
		}
		parts[i] = typedQuote(spec.Name) + " AS (" + body + ")"
	}
	return " WINDOW " + strings.Join(parts, ", "), nil
}
