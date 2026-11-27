package plan

import "github.com/0xJean/tabula"

// ArrayExpr constructs a nonempty one-dimensional array. All elements must
// have the same SQL type. SQL NULL elements are retained. Use an element-null
// preserving output codec when decoding arrays that can contain NULLs.
func ArrayExpr[V any](first Expr[V], rest ...Expr[V]) Expr[[]V] {
	var result tabula.PGType[[]V]
	if typeID(first.kind).GoType != nil && typeID(first.kind).Element == nil {
		result = ArrayOf(first.kind)
	}
	return ArrayExprAs(result, first, rest...)
}

// ArrayExprAs constructs an array with an explicit result mapping. For nullable
// elements use pgtype.ArrayOfNullable(element), which preserves NULLs including
// those introduced by a LEFT JOIN. Every element must have the exact SQL type
// of the declared array element; the array itself is always non-null.
func ArrayExprAs[R, V any](result tabula.PGType[R], first Expr[V], rest ...Expr[V]) Expr[R] {
	n := scalarNode{syntax: scalarArray, name: "ARRAY", args: scalarOperands(first, rest), nulls: nullNever}
	if typeID(result).Element == nil || !sameSQLType(*typeID(result).Element, typeID(first.kind)) {
		n.err = scalarError("ARRAY", "an array result with the exact input element type", n.args)
	}
	for _, a := range n.args {
		if !reviewedType(a.kind) || a.kind.Element != nil || !sameSQLType(a.kind, typeID(first.kind)) {
			n.err = scalarError("ARRAY", "matching reviewed scalar element types", n.args)
		}
	}
	return scalarResult(result, n)
}

// ArrayCat concatenates arrays. A non-null array is preserved when the other
// operand is SQL NULL. The element types must match exactly.
func ArrayCat[V any](a, b Expr[[]V]) Expr[[]V] {
	v := sameTypeOperator("||", a.kind, a, b, nullAll)
	if typeID(a.kind).Element == nil {
		n := v.part.(scalarNode)
		n.err = scalarError("ArrayCat", "array operands", n.args)
		v.part = n
	}
	return v
}

// Cardinality returns the total element count, including zero for an empty
// non-null array. A SQL NULL array yields SQL NULL.
func Cardinality[A any](array Expr[A]) Expr[int32] {
	n := scalarNode{name: "cardinality", args: scalarArgs(array), functions: []string{"pg_catalog.cardinality(anyarray)"}}
	if typeID(array.kind).Element == nil || !reviewedType(typeID(array.kind)) {
		n.err = scalarError(n.name, "a reviewed one-dimensional array", n.args)
	}
	return scalarResult(Int4(), n)
}

// ArrayRemove removes every matching element using null-aware equality. A NULL
// element removes NULL elements; only a NULL array makes the result NULL.
func ArrayRemove[A, V any](array Expr[A], element Expr[V]) Expr[A] {
	n := scalarNode{name: "array_remove", args: scalarArgs(array, element), nulls: nullFirst,
		functions: []string{"pg_catalog.array_remove(anycompatiblearray,anycompatible)"}}
	if !arrayElementMatches(typeID(array.kind), typeID(element.kind)) {
		n.err = scalarError(n.name, "an array and its exact comparable element type", n.args)
	}
	return scalarResult(array.kind, n)
}

func arrayElementMatches(array, element catalogType) bool {
	return array.Element != nil && comparableScalar(element, false) && sameSQLType(*array.Element, element)
}

func arrayPredicate[A any](op string, a, b Expr[A]) Pred {
	n := scalarNode{syntax: scalarOperator, name: op, args: scalarArgs(a, b)}
	if !sameSQLType(typeID(a.kind), typeID(b.kind)) || typeID(a.kind).Element == nil || !comparableScalar(*typeID(a.kind).Element, false) {
		n.err = scalarError(op, "matching arrays with native element equality", n.args)
	}
	return scalarResult(Bool(), n)
}

// ArrayContains tests containment; duplicate elements do not change membership.
func ArrayContains[A any](a, b Expr[A]) Pred { return arrayPredicate("@>", a, b) }

// ArrayContainedBy tests whether every element in a occurs in b.
func ArrayContainedBy[A any](a, b Expr[A]) Pred { return arrayPredicate("<@", a, b) }

// ArrayOverlaps tests whether arrays have at least one element in common.
func ArrayOverlaps[A any](a, b Expr[A]) Pred { return arrayPredicate("&&", a, b) }

// NotEqualAll uses native <> ALL: an empty array yields true, a match yields
// false, and otherwise NULL arrays/elements can yield SQL NULL.
// Its array representation may preserve nullable elements; the catalog element
// type must still match the scalar exactly.
func NotEqualAll[V, A any](value Expr[V], array Expr[A]) Pred {
	n := scalarNode{syntax: scalarAll, name: "<> ALL", args: scalarArgs(value, array), nulls: nullAlways}
	if !arrayElementMatches(typeID(array.kind), typeID(value.kind)) {
		n.err = scalarError(n.name, "a value and its exact comparable array type", n.args)
	}
	return scalarResult(Bool(), n)
}
