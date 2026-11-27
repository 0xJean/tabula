package plan

import (
	"fmt"
	"slices"

	"github.com/0xJean/tabula"
)

func appendComparisonRequirements(dst []tabula.ComparisonRequirement, values ...tabula.ComparisonRequirement) []tabula.ComparisonRequirement {
	for _, v := range values {
		if !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}

func readComparable(t catalogType) bool { return resultComparable(t) }

func readResultProof(q ReadSelect, sources []rowSource, order []RowTerm) ([]tabula.KeyRequirement, []tabula.ComparisonRequirement, error) {
	if len(q.group) == 0 && readHasAggregate(q) {
		return nil, nil, fmt.Errorf("tabula: a scalar aggregate has no keyset continuation")
	}
	var unique []expressionPart
	switch {
	case len(q.distinctOn) > 0:
		for _, v := range q.distinctOn {
			unique = append(unique, v.part)
		}
	case q.distinct:
		for _, p := range q.project {
			unique = append(unique, p.part)
		}
	case len(q.group) > 0:
		for _, g := range q.group {
			if isNilBound(g) {
				return nil, nil, fmt.Errorf("tabula: invalid grouped result key")
			}
			unique = append(unique, g.groupOperand().part)
		}
	}
	var keys []tabula.KeyRequirement
	if len(unique) == 0 {
		for _, src := range sources {
			if len(src.keys) == 0 {
				return nil, nil, fmt.Errorf("tabula: keyset needs a complete key for occurrence %q", src.rel.alias)
			}
			for _, key := range src.keys {
				found := false
				for _, term := range order {
					f, ok := term.orderValue().part.(*fieldRef)
					if ok && f.source == src.rel && f.name == key {
						unique = append(unique, f)
						found = true
						break
					}
				}
				if !found {
					return nil, nil, fmt.Errorf("tabula: complete key requires %s.%s in the order", src.rel.alias, key)
				}
			}
			keys = appendKeyRequirements(keys, tabula.KeyRequirement{Relation: src.rel.table.SQL(), Columns: src.keys})
		}
	}
	for _, key := range unique {
		if !slices.ContainsFunc(order, func(term RowTerm) bool { return sameExpression(key, term.orderValue().part) }) {
			return nil, nil, fmt.Errorf("tabula: order requires the complete result key")
		}
	}
	if err := checkGroupedOrderValues(q.group, order); err != nil {
		return nil, nil, err
	}
	comparisons, err := readOrderComparisons(order)
	if err == nil {
		windowKeys, windowComparisons, windowErr := readWindowProof(q, sources, order)
		keys = appendKeyRequirements(keys, windowKeys...)
		comparisons = appendComparisonRequirements(comparisons, windowComparisons...)
		err = windowErr
	}
	return keys, comparisons, err
}

func readOrderComparisons(order []RowTerm) ([]tabula.ComparisonRequirement, error) {
	var comparisons []tabula.ComparisonRequirement
	var fields []tabula.ComparisonRequirement
	var visit func(expressionPart) error
	visit = func(part expressionPart) error {
		if a, ok := part.(aggregateRoot); ok {
			if err := repeatableReadAggregate(a.aggregateRoot()); err != nil {
				return err
			}
		}
		traits, ok := expressionStructure(part)
		if !ok {
			return fmt.Errorf("tabula: unknown key expression")
		}
		if f, ok := part.(*fieldRef); ok {
			fields = appendComparisonRequirements(fields, tabula.ComparisonRequirement{Relation: f.source.table.SQL(), Column: f.name})
		}
		for _, child := range traits.children {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	for _, term := range order {
		fields = nil
		value := term.orderValue()
		if !readComparable(value.kind) || !repeatableExpression(value.part) {
			return nil, fmt.Errorf("tabula: keyset order requires a repeatable native comparable expression")
		}
		if err := visit(value.part); err != nil {
			return nil, err
		}
		comparisons = appendComparisonRequirements(comparisons, fields...)
		// Separate order terms can use different deterministic collations. A
		// compound term conservatively requires compatible physical inputs.
		for i, field := range fields {
			for _, peer := range fields[i+1:] {
				field.WithRelation, field.WithColumn = peer.Relation, peer.Column
				comparisons = appendComparisonRequirements(comparisons, field)
			}
		}
	}
	return comparisons, nil
}

func readWinnerProof(q ReadSelect, sources []rowSource) ([]tabula.KeyRequirement, []tabula.ComparisonRequirement, error) {
	var keys []tabula.KeyRequirement
	var comparisons []tabula.ComparisonRequirement
	var err error
	if len(q.group) == 0 && readHasAggregate(q) {
		comparisons, err = readOrderComparisons(q.order)
	} else {
		q.distinct, q.distinctOn = false, nil
		keys, comparisons, err = readResultProof(q, sources, q.order)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("tabula: DISTINCT ON winner: %w", err)
	}
	return keys, comparisons, nil
}

// Explicit aggregate ordering must determine each emitted value. Equal sort
// keys with different values can otherwise change an array or string boundary.
func repeatableReadAggregate(a aggregateCall) error {
	switch a.fn {
	case "array_agg", "string_agg":
		for _, arg := range a.args {
			if _, constant := arg.part.(*typedInput); !constant && !readOrderDeterminesValue(arg.kind) {
				return fmt.Errorf("tabula: cursor aggregate cannot determine an equal input's representative")
			}
			if !slices.ContainsFunc(a.order, func(term RowTerm) bool { return sameExpression(term.orderValue().part, arg.part) }) {
				if _, ok := arg.part.(*typedInput); !ok {
					return fmt.Errorf("tabula: cursor aggregate order must determine every input value")
				}
			}
		}
	case "min", "max":
		for _, arg := range a.args {
			if _, constant := arg.part.(*typedInput); !constant && !readOrderDeterminesValue(arg.kind) {
				return fmt.Errorf("tabula: cursor aggregate cannot determine an equal input's representative")
			}
		}
	case "sum", "avg":
		for _, arg := range a.args {
			if arg.kind.Name == "float4" || arg.kind.Name == "float8" {
				return fmt.Errorf("tabula: floating aggregate cannot prove a repeatable cursor order")
			}
		}
	}
	return nil
}

// Native equality can identify different observable representations: numeric
// scales, floating signed zero, interval units and padded character values.
// An outer expression can distinguish whichever MIN/MAX or ARRAY_AGG picked.
func readOrderDeterminesValue(kind catalogType) bool {
	if kind.Element != nil {
		return readOrderDeterminesValue(*kind.Element)
	}
	if !readComparable(kind) {
		return false
	}
	switch kind.Name {
	case "numeric", "float4", "float8", "interval", "bpchar", "jsonb":
		return false
	}
	return true
}
