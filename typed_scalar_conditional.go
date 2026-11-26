package tabula

// Coalesce returns the first non-NULL operand. All operands must have the same
// PostgreSQL type. It is non-null when at least one operand is non-null.
func Coalesce[V any](first Expr[V], rest ...Expr[V]) Expr[V] {
	return conditional("COALESCE", first, rest, nullAll, false)
}

// Greatest returns the greatest non-NULL operand using native ordering.
func Greatest[V any](first Expr[V], rest ...Expr[V]) Expr[V] {
	return conditional("GREATEST", first, rest, nullAll, true)
}

// Least returns the least non-NULL operand using native ordering.
func Least[V any](first Expr[V], rest ...Expr[V]) Expr[V] {
	return conditional("LEAST", first, rest, nullAll, true)
}

// NullIf returns SQL NULL when its operands compare equal, or the first value.
// Its operands require a reviewed built-in equality operator.
// An array-valued first operand must not contain a volatile expression; scalar
// operands and a volatile second array operand are evaluated once per row.
func NullIf[V any](a, b Expr[V]) Expr[V] {
	v := conditional("NULLIF", a, []Expr[V]{b}, nullAlways, true)
	if a.kind.id.element != nil && !statementStableScalar(a.part) {
		n := v.part.(scalarNode)
		n.err = scalarError("NULLIF", "an array first operand without VOLATILE expressions", n.args)
		v.part = n
	}
	return v
}

func statementStableScalar(p expressionPart) bool {
	t, ok := expressionStructure(p)
	if !ok || t.stability == expressionVolatile {
		return false
	}
	for _, child := range t.children {
		if !statementStableScalar(child) {
			return false
		}
	}
	return true
}

func conditional[V any](name string, first Expr[V], rest []Expr[V], nulls scalarNullRule, compare bool) Expr[V] {
	n := scalarNode{syntax: scalarConditional, name: name, args: scalarOperands(first, rest), nulls: nulls}
	for _, a := range n.args {
		if !sameSQLType(first.kind.id, a.kind) || compare && !comparableScalar(a.kind, name != "NULLIF") {
			n.err = scalarError(name, "matching catalog types with the required native comparison", n.args)
		}
	}
	return scalarResult(first.kind, n)
}

// CaseArm pairs a boolean condition with a result. SQL NULL conditions fall
// through to the next arm. Construct arms with When.
type CaseArm[V any] struct {
	condition Pred
	value     Expr[V]
}

// When declares one typed CASE arm.
func When[V any](condition Pred, value Expr[V]) CaseArm[V] {
	return CaseArm[V]{condition, value}
}

// CaseExpr is a CASE declaration awaiting its explicit fallback.
type CaseExpr[V any] struct{ arms []CaseArm[V] }

// Case starts a conditional expression with at least one arm.
func Case[V any](first CaseArm[V], rest ...CaseArm[V]) CaseExpr[V] {
	return CaseExpr[V]{append([]CaseArm[V]{first}, rest...)}
}

// Else supplies the fallback. Every result arm must have its exact catalog type.
func (c CaseExpr[V]) Else(value Expr[V]) Expr[V] { return c.finish(value) }

// ElseNull supplies a typed SQL NULL fallback.
func (c CaseExpr[V]) ElseNull() Expr[V] {
	var kind PGType[V]
	if len(c.arms) > 0 {
		kind = c.arms[0].value.kind
	}
	return c.finish(TypedNull(kind))
}

func (c CaseExpr[V]) finish(otherwise Expr[V]) Expr[V] {
	n := scalarNode{syntax: scalarCase, name: "CASE", nulls: nullCase}
	for _, arm := range c.arms {
		n.args = append(n.args, arm.condition.scalarOperand(), arm.value.scalarOperand())
		if !sameSQLType(arm.value.kind.id, otherwise.kind.id) || checkRowBool(arm.condition) != nil {
			n.err = scalarError("CASE", "boolean conditions and matching result catalog types", n.args)
		}
	}
	n.args = append(n.args, otherwise.scalarOperand())
	if len(c.arms) == 0 {
		n.err = scalarError("CASE", "at least one declared arm", n.args)
	}
	return scalarResult(otherwise.kind, n)
}
