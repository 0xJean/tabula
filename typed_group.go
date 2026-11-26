package tabula

import (
	"fmt"
	"slices"
)

// GroupExpr retains a computed expression for RowsSpec.GroupBy. It cannot be
// used as a physical column in a selection, source policy or write target.
func GroupExpr[V any](value Expr[V]) RowColumn { return RowColumn{expr: value.scalarOperand()} }

// resultComparable admits only closed native equality and ordering families.
// Physical collation compatibility is additionally verified for cursor proofs.
func resultComparable(t catalogType) bool {
	if t.element != nil {
		return resultComparable(*t.element)
	}
	if t.schema != "pg_catalog" || t.goType == nil {
		return false
	}
	switch t.name {
	case "bool", "int2", "int4", "int8", "oid", "float4", "float8", "numeric",
		"text", "varchar", "bpchar", "bytea", "uuid", "date", "time", "timetz",
		"timestamp", "timestamptz", "interval", "jsonb", "inet", "cidr", "macaddr", "macaddr8":
		return true
	}
	return false
}

// Existing physical custom mappings remain catalog-verified. New expression
// comparisons use the closed native family above.
func groupOrderComparable(v scalarOperand) bool {
	if f, ok := v.part.(*fieldRef); ok && f != nil && f.kind.goType != nil && f.kind.schema != "pg_catalog" {
		return true
	}
	return resultComparable(v.kind)
}

func checkGroupKeys(groups []scalarOperand, sources map[*occurrence]bool) error {
	for i, group := range groups {
		if !groupOrderComparable(group) {
			return fmt.Errorf("tabula: GROUP BY operand %d requires a comparable native type, got %s", i+1, group.kind.qualified())
		}
		if err := checkRowExpr(group.part, nil); err != nil {
			return fmt.Errorf("tabula: GROUP BY operand %d: %w", i+1, err)
		}
		if err := checkExpressionScope(group.part, sources); err != nil {
			return err
		}
		for _, earlier := range groups[:i] {
			if sameExpression(earlier.part, group.part) {
				return fmt.Errorf("tabula: duplicate GROUP BY expression")
			}
		}
	}
	return nil
}

func checkExpressionScope(p expressionPart, sources map[*occurrence]bool) error {
	t, ok := expressionStructure(p)
	if !ok {
		return fmt.Errorf("tabula: unsupported or undeclared row expression")
	}
	if f, ok := p.(*fieldRef); ok {
		if _, present := sources[f.source]; !present {
			return fmt.Errorf("tabula: expression field is outside its relation scope")
		}
	}
	for _, child := range t.children {
		if err := checkExpressionScope(child, sources); err != nil {
			return err
		}
	}
	return nil
}

func hasAggregate(p expressionPart) bool {
	t, ok := expressionStructure(p)
	return ok && (t.aggregate || slices.ContainsFunc(t.children, hasAggregate))
}

func groupDetermined(p expressionPart, groups []scalarOperand) error {
	if slices.ContainsFunc(groups, func(g scalarOperand) bool { return sameExpression(g.part, p) }) {
		return nil
	}
	t, ok := expressionStructure(p)
	if !ok {
		return fmt.Errorf("tabula: unsupported or undeclared row expression")
	}
	if t.aggregate {
		return nil
	}
	if f, ok := p.(*fieldRef); ok {
		return fmt.Errorf("tabula: ungrouped field %q on occurrence %q", f.name, f.source.alias)
	}
	for _, child := range t.children {
		if err := groupDetermined(child, groups); err != nil {
			return err
		}
	}
	return nil
}

func groupingExpressions(project []Projection, having []Pred, order []RowTerm, groups []scalarOperand) (bool, error) {
	parts := make([]expressionPart, 0, len(project)+len(having)+len(order))
	for _, p := range project {
		parts = append(parts, p.part)
	}
	for _, p := range having {
		parts = append(parts, p.part)
	}
	for _, term := range order {
		parts = append(parts, term.orderValue().part)
	}
	aggregate := len(groups) > 0 || len(having) > 0 || slices.ContainsFunc(parts, hasAggregate)
	for _, part := range parts {
		if err := checkRowExpr(part, func(expressionPart) error { return nil }); err != nil {
			return false, err
		}
		if aggregate {
			if err := groupDetermined(part, groups); err != nil {
				return false, err
			}
		}
	}
	return aggregate, nil
}

func rowGroupOperands(groups []RowColumn) []scalarOperand {
	values := make([]scalarOperand, len(groups))
	for i, group := range groups {
		values[i] = group.groupOperand()
	}
	return values
}
