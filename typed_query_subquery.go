package tabula

import (
	"fmt"
	"reflect"
)

// ComparisonOp is a closed native comparison used by ANY and ALL subqueries.
type ComparisonOp uint8

const (
	// CompareEqual selects native equality.
	CompareEqual ComparisonOp = iota + 1
	// CompareNotEqual selects native inequality.
	CompareNotEqual
	// CompareLess selects native less-than.
	CompareLess
	// CompareLessEqual selects native less-than-or-equal.
	CompareLessEqual
	// CompareGreater selects native greater-than.
	CompareGreater
	// CompareGreaterEqual selects native greater-than-or-equal.
	CompareGreaterEqual
)

func (o ComparisonOp) sql() string {
	return map[ComparisonOp]string{CompareEqual: "=", CompareNotEqual: "<>", CompareLess: "<", CompareLessEqual: "<=", CompareGreater: ">", CompareGreaterEqual: ">="}[o]
}

// ScalarSubquery returns the sole value or SQL NULL for an empty result.
// Multiple result rows retain PostgreSQL's cardinality error.
func ScalarSubquery[V any](query ValueSelect[V]) Expr[V] {
	return Expr[V]{queryExpression{query: query.query, mode: "scalar", result: query.value.kind.id}, query.value.kind, true}
}

// ExistsQuery reports whether the declared query has any result row.
func ExistsQuery(query ReadSelect) Pred {
	return Pred{queryExpression{query: query, mode: "exists", result: Bool().id}, Bool(), false}
}

// InQuery tests native equality against a one-column query, retaining SQL NULL.
func InQuery[V any](value Expr[V], query ValueSelect[V]) Pred {
	return AnyQuery(CompareEqual, value, query)
}

// AnyQuery compares against any row. Empty input yields false.
func AnyQuery[V any](op ComparisonOp, value Expr[V], query ValueSelect[V]) Pred {
	return quantifiedQuery("ANY", op, value, query)
}

// AllQuery compares against every row. Empty input yields true.
func AllQuery[V any](op ComparisonOp, value Expr[V], query ValueSelect[V]) Pred {
	return quantifiedQuery("ALL", op, value, query)
}
func quantifiedQuery[V any](mode string, op ComparisonOp, value Expr[V], query ValueSelect[V]) Pred {
	return Pred{queryExpression{query: query.query, mode: mode, result: Bool().id, left: value.scalarOperand(), comparison: op}, Bool(), true}
}

// ArraySubquery builds an ordered one-dimensional array. Empty input produces
// a non-NULL empty array. Nullable elements require ArraySubqueryAs.
func ArraySubquery[V any](query ValueSelect[V]) Expr[[]V] {
	var kind PGType[[]V]
	if query.value.kind.id.goType != nil && query.value.kind.id.element == nil {
		kind = ArrayOf(query.value.kind)
	}
	return ArraySubqueryAs(kind, query)
}

// ArraySubqueryAs supplies an exact array mapping, including nullable elements.
func ArraySubqueryAs[R, V any](result PGType[R], query ValueSelect[V]) Expr[R] {
	return Expr[R]{queryExpression{query: query.query, mode: "array", result: result.id}, result, false}
}

type queryExpression struct {
	query      ReadSelect
	mode       string
	result     catalogType
	left       scalarOperand
	comparison ComparisonOp
}

func (e queryExpression) expressionTraits() expressionTraits {
	t := expressionTraits{identity: "subquery/" + e.mode, stability: expressionStable}
	if e.left.part != nil {
		t.children = []expressionPart{e.left.part}
	}
	return t
}
func (e queryExpression) nullableAfterJoin(map[*occurrence]bool) bool {
	return e.mode != "exists" && e.mode != "array"
}
func (e queryExpression) render(c *typedCompiler) (string, error) {
	if c.block == nil {
		return "", fmt.Errorf("tabula: subquery requires a lexical query block")
	}
	q := e.query
	if e.mode == "exists" && len(q.project) == 0 && q.set == nil {
		q.project = []Projection{{id: new(queryFieldIdentity), field: ShapeField{Name: "exists", Type: Int4().id.qualified()}, part: queryOne{}, kind: Int4().id, compatible: true}}
	}
	var lhs string
	var err error
	if e.left.part != nil {
		lhs, err = e.left.part.render(c)
		if err != nil {
			return "", err
		}
	}
	result, err := q.renderStatement(&statementContext{compiler: c, block: c.block, position: statementScalar})
	if err != nil {
		return "", err
	}
	if result.effects.bits&EffectWrite != 0 {
		return "", fmt.Errorf("tabula: a subquery cannot contain write effects")
	}
	c.queryShapes = append(c.queryShapes, result.shape)
	if e.mode == "exists" {
		return "EXISTS (" + result.sql + ")", nil
	}
	if len(result.fields) != 1 {
		return "", fmt.Errorf("tabula: scalar query requires exactly one output")
	}
	field := result.fields[0].value
	switch e.mode {
	case "scalar":
		if !sameQueryType(e.result, field.kind) {
			return "", fmt.Errorf("tabula: scalar query result type mismatch")
		}
		return "(" + result.sql + ")", nil
	case "array":
		if e.result.element == nil || field.kind.element != nil || !sameQueryType(*e.result.element, field.kind) {
			return "", fmt.Errorf("tabula: ARRAY query requires an exact scalar element mapping")
		}
		elem := e.result.goType
		if elem == nil || elem.Kind() != reflect.Slice {
			return "", fmt.Errorf("tabula: ARRAY query requires an array codec")
		}
		representation := elem.Elem()
		if representation != field.kind.goType && (representation.Kind() != reflect.Pointer || representation.Elem() != field.kind.goType) {
			return "", fmt.Errorf("tabula: ARRAY query element Go representation mismatch")
		}
		if field.nullable && (representation.Kind() != reflect.Pointer || representation.Elem() != field.kind.goType) {
			return "", fmt.Errorf("tabula: nullable ARRAY query element requires ArrayOfNullable")
		}
		return "ARRAY(" + result.sql + ")", nil
	default:
		op := e.comparison.sql()

		if op == "" || !sameSQLType(e.left.kind, field.kind) || !readComparable(field.kind) {
			return "", fmt.Errorf("tabula: quantified query requires a supported native comparison of exact types")
		}
		if err := requireReadComparison(c, field.kind, map[string]string{"=": "eq", "<>": "ne", "<": "lt", "<=": "le", ">": "gt", ">=": "ge"}[op]); err != nil {
			return "", err
		}
		return "(" + lhs + " OPERATOR(pg_catalog." + op + ") " + e.mode + " (" + result.sql + "))", nil
	}
}
func sameQueryType(a, b catalogType) bool {
	if !sameSQLType(a, b) || a.goType != b.goType || a.encoder != b.encoder {
		return false
	}
	return a.element == nil || sameQueryType(*a.element, *b.element)
}

type queryOne struct{}

func (queryOne) expressionTraits() expressionTraits {
	return expressionTraits{identity: "exists projection"}
}
func (queryOne) nullableAfterJoin(map[*occurrence]bool) bool { return false }
func (queryOne) render(*typedCompiler) (string, error)       { return "1", nil }
