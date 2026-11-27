package plan

import (
	"github.com/0xJean/tabula"

	"slices"
	"strings"
)

func querySetResultKeys(operation string, arms []statementResult, fields []queryResultField) []queryResultKey {
	if len(arms) == 0 {
		return nil
	}
	for i := range fields {
		p := arms[0].fields[i].proof.frozen()
		for _, arm := range arms[1:] {
			p = p.combine(arm.fields[i].proof)
		}
		if operation != "UNION ALL" {
			p.exact = p.exact && readOrderDeterminesValue(fields[i].value.kind)
		}
		p.comparisons = appendComparisonRequirements(p.comparisons, queryFieldComparisons(fields[i])...)
		fields[i].proof = p
	}
	if !strings.HasSuffix(operation, " ALL") {
		key := queryResultKey{}
		for _, field := range fields {
			if field.id == nil || !resultComparable(field.value.kind) {
				return nil
			}
			key.fields = append(key.fields, field.id)
			key.comparisons = appendComparisonRequirements(key.comparisons, queryFieldComparisons(field)...)
		}
		return []queryResultKey{key}
	}
	if operation != "UNION ALL" {
		// INTERSECT ALL and EXCEPT ALL cannot increase first-arm multiplicity.
		return cloneQueryKeys(arms[0].keys)
	}
	var discriminator []int
	for i := range fields {
		allFixed := true
		for _, arm := range arms {
			_, known := queryConstantClass(arm.fields[i].value.part)
			allFixed = allFixed && known
		}
		if allFixed {
			discriminator = append(discriminator, i)
		}
	}
	if len(discriminator) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	for _, arm := range arms {
		var token strings.Builder
		for _, i := range discriminator {
			value, _ := queryConstantClass(arm.fields[i].value.part)
			token.WriteString(value + "/")
		}
		if seen[token.String()] {
			return nil
		}
		seen[token.String()] = true
	}
	key := queryResultKey{}
	for _, i := range discriminator {
		key.fields = append(key.fields, fields[i].id)
	}
	for _, arm := range arms {
		if len(arm.keys) == 0 || len(arm.keys[0].fields) == 0 {
			return nil
		}
		premise := arm.keys[0]
		for _, id := range premise.fields {
			index := slices.IndexFunc(arm.fields, func(f queryResultField) bool { return f.id == id })
			if index < 0 || fields[index].id == nil {
				return nil
			}
			if !slices.Contains(key.fields, fields[index].id) {
				key.fields = append(key.fields, fields[index].id)
			}
		}
		key.keys = appendKeyRequirements(key.keys, premise.keys...)
		key.comparisons = appendComparisonRequirements(key.comparisons, premise.comparisons...)
	}
	return []queryResultKey{key}
}

// Discriminators must be closed declaration-time SQL facts, never current
// Binding values. NULL and boolean constants formed with IS NULL/NOT suffice
// for arbitrary finite tuples without adding a public literal escape.
func queryConstantClass(part expressionPart) (string, bool) {
	switch n := part.(type) {
	case scalarNode:
		if n.syntax == scalarNull {
			return "null", true
		}
	case rowNull:
		value, ok := queryConstantClass(n.value)
		if ok {
			if value == "null" {
				return "true", true
			}
			return "false", true
		}
	case rowNot:
		value, ok := queryConstantClass(n.term.part)
		if ok {
			switch value {
			case "true":
				return "false", true
			case "false":
				return "true", true
			case "null":
				return "null", true
			}
		}
	}
	return "", false
}

func queryFieldComparisons(f queryResultField) []tabula.ComparisonRequirement {
	var comparisons []tabula.ComparisonRequirement
	for i, origin := range f.origins {
		comparisons = appendComparisonRequirements(comparisons, tabula.ComparisonRequirement{Relation: origin.Table.SQL(), Column: origin.Name})
		for _, peer := range f.origins[i+1:] {
			if peer != origin {
				comparisons = appendComparisonRequirements(comparisons, tabula.ComparisonRequirement{Relation: origin.Table.SQL(), Column: origin.Name, WithRelation: peer.Table.SQL(), WithColumn: peer.Name})
			}
		}
	}
	return comparisons
}
