package tabula

import (
	"fmt"
	"slices"
	"strings"
)

func (n writeDeclaration) renderMutation(ctx *statementContext, meta *StatementShape) (string, error) {
	if len(n.where) == 0 && !n.all {
		return "", fmt.Errorf("tabula: a mutation requires a fixed predicate or explicit AllRows")
	}
	c := ctx.compiler
	var sources, matchTerms []string
	for _, match := range n.matches {
		text, _, keys, err := renderQuerySource(ctx, match.source, false)
		if err != nil {
			return "", err
		}
		sources = append(sources, text)
		if err := checkRowExpr(match.on.part, nil); err != nil {
			return "", err
		}
		on, err := renderRowPredicate(c, match.on)
		if err != nil {
			return "", err
		}
		matchTerms = append(matchTerms, on)
		if _, result := match.source.(QuerySource); !result {
			src, err := match.source.readSource()
			if err != nil {
				return "", err
			}
			terms, err := src.where(c, nil)
			if err != nil {
				return "", err
			}
			matchTerms = append(matchTerms, terms...)
		}
		if n.command == "UPDATE" || n.returnsMatchValues() {
			if err := proveWriteMatch(match, keys, meta); err != nil {
				return "", err
			}
		}
	}
	query := "DELETE FROM " + n.targetSQL(c)
	if n.command == "UPDATE" {
		set, err := n.renderAssignments(c, n.set, meta)
		if err != nil {
			return "", err
		}
		query = "UPDATE " + n.targetSQL(c) + " SET " + set
	} else {
		meta.Privileges = append(meta.Privileges, DefinedPrivilege{Table: n.target.rel.table.sql(), Privilege: "DELETE"})
	}
	if len(sources) != 0 {
		clause := " FROM "
		if n.command == "DELETE" {
			clause = " USING "
		}
		query += clause + strings.Join(sources, ", ")
	}
	terms, err := n.mutationWhere(c, n.where)
	if err != nil {
		return "", err
	}
	return query + whereSQL(append(terms, matchTerms...)), nil
}

func (n writeDeclaration) returnsMatchValues() bool {
	for _, projection := range n.returning {
		if !writeTargetValue(projection.part, n.target.rel) {
			return true
		}
	}
	return false
}

func writeTargetValue(part expressionPart, target *occurrence) bool {
	switch value := part.(type) {
	case *fieldRef:
		return value != nil && value.source == target
	case queryResultRef, queryExpression:
		// A nested query is a separate lexical boundary and can explicitly
		// correlate to the selected match. Retain its matching premise.
		return false
	}
	traits, ok := expressionStructure(part)
	if !ok {
		return false
	}
	for _, child := range traits.children {
		if !writeTargetValue(child, target) {
			return false
		}
	}
	return true
}

func proveWriteMatch(match writeMatch, keys []queryResultKey, meta *StatementShape) error {
	pairs := writeEqualities(match.on.part)
	if result, ok := match.source.(QuerySource); ok {
		covered := make(map[*queryFieldIdentity]bool)
		for _, pair := range pairs {
			for _, side := range [][2]expressionPart{pair, {pair[1], pair[0]}} {
				ref, ok := side[0].(queryResultRef)
				if ok && ref.source == result.occurrence && independentMatchValue(side[1], nil, result.occurrence) {
					field, declared := ref.field()
					if declared && readComparable(field.value.kind) {
						covered[ref.id] = true
					}
				}
			}
		}
		for _, key := range keys {
			if !slices.ContainsFunc(key.fields, func(id *queryFieldIdentity) bool { return !covered[id] }) {
				meta.Keys = appendKeyRequirements(meta.Keys, key.keys...)
				meta.Comparisons = appendComparisonRequirements(meta.Comparisons, key.comparisons...)
				return nil
			}
		}
		return fmt.Errorf("tabula: value-bearing write source needs equality covering a proven result key; use a grouped or distinct Derived source")
	}
	src, err := match.source.readSource()
	if err != nil {
		return err
	}
	var columns []string
	for _, pair := range pairs {
		for _, side := range [][2]expressionPart{pair, {pair[1], pair[0]}} {
			field, ok := side[0].(*fieldRef)
			if ok && field != nil && field.source == src.rel && !field.nullable && readComparable(field.kind) &&
				independentMatchValue(side[1], src.rel, nil) && !slices.Contains(columns, field.name) {
				columns = append(columns, field.name)
				meta.Comparisons = appendComparisonRequirements(meta.Comparisons,
					ComparisonRequirement{Relation: src.rel.table.sql(), Column: field.name})
			}
		}
	}
	if len(columns) == 0 {
		return fmt.Errorf("tabula: value-bearing write source needs equality covering an immediate non-null unique key")
	}
	// This is a catalog premise, not an application uniqueness flag. Readiness
	// must establish an actual valid immediate non-null unique key covered by it.
	meta.UniqueMatches = append(meta.UniqueMatches, UniqueMatchRequirement{Relation: src.rel.table.sql(), Columns: columns})
	return nil
}

func independentMatchValue(part expressionPart, physical *occurrence, result *queryOccurrence) bool {
	switch n := part.(type) {
	case *typedInput:
		return n != nil
	case *fieldRef:
		return n != nil && n.source != physical
	case queryResultRef:
		return n.source != result && repeatableExpression(n)
	default:
		return false
	}
}

func writeEqualities(part expressionPart) [][2]expressionPart {
	switch n := part.(type) {
	case comparison:
		if n.op == "=" && n.matchingType {
			return [][2]expressionPart{{n.left, n.right}}
		}
	case conjunction:
		var pairs [][2]expressionPart
		for _, predicate := range n {
			pairs = append(pairs, writeEqualities(predicate.part)...)
		}
		return pairs
	}
	return nil
}
