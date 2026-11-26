package tabula

import (
	"fmt"
	"slices"
)

// KeyRequirement records a complete immediate primary key used to prove a
// result order. Relation is a quoted, qualified scanned relation, including
// its descendants. Catalog verification rejects ordinary inheritance and
// accepts a valid parent-wide primary key on a partitioned relation.
type KeyRequirement struct {
	Relation string
	Columns  []string
}

// ComparisonRequirement requires native comparison semantics and a
// deterministic collation for a physical column used by a result order.
// When WithRelation and WithColumn are set, both columns also require
// compatible implicit collations within one expression.
type ComparisonRequirement struct {
	Relation, Column         string
	WithRelation, WithColumn string `json:",omitempty"`
}

func appendKeyRequirements(dst []KeyRequirement, values ...KeyRequirement) []KeyRequirement {
	for _, v := range values {
		if !slices.ContainsFunc(dst, func(k KeyRequirement) bool { return k.Relation == v.Relation && slices.Equal(k.Columns, v.Columns) }) {
			v.Columns = slices.Clone(v.Columns)
			dst = append(dst, v)
		}
	}
	return dst
}
func appendComparisonRequirements(dst []ComparisonRequirement, values ...ComparisonRequirement) []ComparisonRequirement {
	for _, v := range values {
		if !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}

func readComparable(t catalogType) bool { return resultComparable(t) }

func readResultProof(q ReadSelect, sources []rowSource, order []RowTerm) ([]KeyRequirement, []ComparisonRequirement, error) {
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
	var keys []KeyRequirement
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
			keys = appendKeyRequirements(keys, KeyRequirement{Relation: src.rel.table.sql(), Columns: src.keys})
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
	return keys, comparisons, err
}

func readOrderComparisons(order []RowTerm) ([]ComparisonRequirement, error) {
	var comparisons []ComparisonRequirement
	var fields []ComparisonRequirement
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
			fields = appendComparisonRequirements(fields, ComparisonRequirement{Relation: f.source.table.sql(), Column: f.name})
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

func readWinnerProof(q ReadSelect, sources []rowSource) ([]KeyRequirement, []ComparisonRequirement, error) {
	var keys []KeyRequirement
	var comparisons []ComparisonRequirement
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
			if arg.kind.name == "float4" || arg.kind.name == "float8" {
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
	if kind.element != nil {
		return readOrderDeterminesValue(*kind.element)
	}
	if !readComparable(kind) {
		return false
	}
	switch kind.name {
	case "numeric", "float4", "float8", "interval", "bpchar", "jsonb":
		return false
	}
	return true
}
