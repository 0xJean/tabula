package plan

import (
	"slices"

	"github.com/0xJean/tabula"
)

// queryValueProof describes a reproducible value, not a unique row. It is
// computed in the producing block. Missing proof fails closed at the consumer.
// exact distinguishes one representation from an equality-class representative.
type queryValueProof struct {
	valid, exact bool
	keys         []tabula.KeyRequirement
	comparisons  []tabula.ComparisonRequirement
}

func (p queryValueProof) frozen() queryValueProof {
	p.keys = appendKeyRequirements(nil, p.keys...)
	p.comparisons = slices.Clone(p.comparisons)
	return p
}

func (p queryValueProof) combine(other queryValueProof) queryValueProof {
	p.valid = p.valid && other.valid
	p.exact = p.exact && other.exact
	p.keys = appendKeyRequirements(p.keys, other.keys...)
	p.comparisons = appendComparisonRequirements(p.comparisons, other.comparisons...)
	return p
}

func queryProjectionKeys(c *typedCompiler, q ReadSelect, fields []queryResultField) []queryResultKey {
	var key queryResultKey
	var values []expressionPart
	switch {
	case q.distinct:
		for _, field := range fields {
			values = append(values, field.value.part)
		}
	case len(q.group) > 0:
		for _, g := range q.group {
			values = append(values, g.groupOperand().part)
		}
	case len(q.distinctOn) > 0:
		for _, v := range q.distinctOn {
			values = append(values, v.part)
		}
	default:
		if readHasAggregate(q) {
			return nil
		}
		for _, source := range append([]ReadSource{q.from}, queryJoinSources(q.joins)...) {
			if source == nil {
				continue
			}
			if result, ok := source.(QuerySource); ok {
				var selected *queryResultKey
				for _, candidate := range c.block.resultKeys[result.occurrence] {
					if len(candidate.fields) == 0 {
						continue
					}
					covered := true
					for _, id := range candidate.fields {
						covered = covered && queryProjectedIdentity(fields, queryResultRef{result.occurrence, id}) != nil
					}
					if covered {
						selected = &candidate
						break
					}
				}
				if selected == nil {
					return nil
				}
				for _, id := range selected.fields {
					values = append(values, queryResultRef{result.occurrence, id})
				}
				key.keys = appendKeyRequirements(key.keys, selected.keys...)
				key.comparisons = appendComparisonRequirements(key.comparisons, selected.comparisons...)
				continue
			}
			src, err := source.readSource()
			if err != nil || len(src.keys) == 0 {
				return nil
			}
			for _, name := range src.keys {
				var found expressionPart
				for _, field := range fields {
					if f, ok := field.value.part.(*fieldRef); ok && f.source == src.rel && f.name == name {
						found = f
						break
					}
				}
				if found == nil {
					return nil
				}
				values = append(values, found)
			}
			key.keys = appendKeyRequirements(key.keys, tabula.KeyRequirement{Relation: src.rel.table.SQL(), Columns: src.keys})
		}
	}
	for _, value := range values {
		id := queryProjectedIdentity(fields, value)
		if id == nil {
			return nil
		}
		key.fields = append(key.fields, id)
		key.comparisons = appendComparisonRequirements(key.comparisons, queryValueComparisons(c, value)...)
	}
	if len(key.fields) == 0 {
		return nil
	}
	return []queryResultKey{key}
}

func queryValueComparisons(c *typedCompiler, part expressionPart) []tabula.ComparisonRequirement {
	if ref, ok := part.(queryResultRef); ok {
		field, err := c.block.resolveResult(ref.source, ref.id, 0)
		if err == nil {
			return appendComparisonRequirements(queryFieldComparisons(field), field.proof.comparisons...)
		}
	}
	comparisons := queryFieldComparisons(queryResultField{origins: queryResolvedOrigins(c, part)})
	traits, _ := expressionStructure(part)
	for _, child := range traits.children {
		comparisons = appendComparisonRequirements(comparisons, queryValueComparisons(c, child)...)
	}
	return comparisons
}

func queryProjectedIdentity(fields []queryResultField, value expressionPart) *queryFieldIdentity {
	for _, field := range fields {
		if sameExpression(value, field.value.part) && field.id != nil && resultComparable(field.value.kind) {
			return field.id
		}
	}
	return nil
}

func queryProveFields(c *typedCompiler, q ReadSelect, fields []queryResultField) {
	groups := make([]scalarOperand, 0, len(q.group))
	for _, g := range q.group {
		groups = append(groups, g.groupOperand())
	}
	for i := range fields {
		f := &fields[i]
		f.proof = proveQueryValue(c, q, f.value.part)
		f.proof.valid = f.proof.valid && f.stability == expressionImmutable && resultComparable(f.value.kind)
		if len(groups) > 0 {
			f.proof.valid = f.proof.valid && stableGroupValue(f.value.part, groups, false)
			if slices.ContainsFunc(groups, func(g scalarOperand) bool { return sameExpression(g.part, f.value.part) }) {
				f.proof.exact = f.proof.exact && readOrderDeterminesValue(f.value.kind)
			}
		}
		if q.distinct {
			f.proof.exact = f.proof.exact && readOrderDeterminesValue(f.value.kind)
		}
	}
	if q.limit != nil {
		proof := queryLimitValueProof(c, q, fields)
		for i := range fields {
			fields[i].proof = fields[i].proof.combine(proof)
		}
	}
}

func proveQueryValue(c *typedCompiler, q ReadSelect, part expressionPart) queryValueProof {
	t, ok := expressionStructure(part)
	proof := queryValueProof{valid: ok && t.stability == expressionImmutable, exact: true}
	if !proof.valid {
		return proof
	}
	switch n := part.(type) {
	case queryResultRef:
		f, err := c.block.resolveResult(n.source, n.id, 0)
		if err != nil || f.stability != expressionImmutable {
			return queryValueProof{}
		}
		return f.proof.frozen()
	case queryOuter, queryExpression:
		return queryValueProof{} // no traversal proof from an outer/subquery declaration alone
	case *fieldRef:
		proof.valid = readComparable(n.kind)
		proof.comparisons = []tabula.ComparisonRequirement{{Relation: n.source.table.SQL(), Column: n.name}}
		return proof
	}
	if a, ok := part.(aggregateRoot); ok && repeatableReadAggregate(a.aggregateRoot()) != nil {
		return queryValueProof{}
	}
	if n, ok := part.(windowExpression); ok {
		proof = proof.combine(queryWindowValueProof(c, q, n))
	}
	needExact := true
	switch n := part.(type) {
	case rowNull, membership, rowNot, rowOr, conjunction, windowExpression:
		needExact = false
	case comparison:
		needExact = n.op == "LIKE" || n.op == "ILIKE"
	}
	for _, child := range t.children {
		next := proveQueryValue(c, q, child)
		if needExact && !next.exact {
			next.valid = false
		}
		proof = proof.combine(next)
	}
	// Compound expressions can combine implicit collations. Keep this premise
	// separate from the deterministic-collation requirement on each origin.
	proof.comparisons = appendComparisonRequirements(proof.comparisons, queryValueComparisons(c, part)...)
	return proof
}

func queryWindowValueProof(c *typedCompiler, q ReadSelect, n windowExpression) queryValueProof {
	proof := queryValueProof{valid: true, exact: true}
	if queryCountStability(q, make(map[*commonTableNode]bool)) != nil {
		return queryValueProof{}
	}
	if a, ok := n.function.(aggregateCall); ok {
		a.order = n.spec.Order
		if repeatableReadAggregate(a) != nil {
			return queryValueProof{}
		}
	}
	if len(q.group) == 0 && readHasAggregate(q) {
		return proof // the input to this window contains at most one aggregate row
	}
	var fields []queryResultField
	for _, arg := range scalarArgs(n.spec.Partition...) {
		fields = append(fields, queryResultField{id: new(queryFieldIdentity), value: arg})
	}
	for _, term := range n.spec.Order {
		fields = append(fields, queryResultField{id: new(queryFieldIdentity), value: term.orderValue()})
	}
	base := q
	base.project, base.order, base.distinctOn, base.distinct = nil, nil, nil, false
	keys := queryProjectionKeys(c, base, fields)
	if len(keys) == 0 {
		return queryValueProof{}
	}
	proof.keys, proof.comparisons = keys[0].keys, keys[0].comparisons
	return proof
}
