package tabula

import "slices"

// Scalar is a declared typed expression in a heterogeneous argument list.
// Its PostgreSQL type, input codec and relation identity stay attached to it.
type Scalar interface {
	scalarOperand() scalarOperand
}

type scalarOperand struct {
	part     expressionPart
	kind     catalogType
	nullable bool
}

func (e Expr[V]) scalarOperand() scalarOperand {
	return scalarOperand{e.part, e.kind.id, e.nullable}
}

func (v scalarOperand) nullableWith(outer map[*occurrence]bool) bool {
	return v.nullable || nullExtended(v.part, outer)
}

// expressionTraits describes SQL structure independently of a renderer's slot
// numbering. identity includes the closed overload, type and grammar options.
type expressionTraits struct {
	identity  string
	children  []expressionPart
	stability expressionStability
	aggregate bool
}

type expressionStability uint8

const (
	expressionImmutable expressionStability = iota
	expressionStable
	expressionVolatile
)

type describedExpression interface {
	expressionPart
	expressionTraits() expressionTraits
	nullableAfterJoin(map[*occurrence]bool) bool
}

// expressionStructure covers the original nodes as well as compound
// expressions. Fields and slots are compared by their declarations below.
func expressionStructure(p expressionPart) (expressionTraits, bool) {
	var t expressionTraits
	switch n := p.(type) {
	case describedExpression:
		return n.expressionTraits(), true
	case *fieldRef:
		return expressionTraits{identity: "field"}, n != nil && n.source != nil
	case *typedInput:
		return expressionTraits{identity: "slot"}, n != nil
	case comparison:
		t.identity, t.children = n.op, []expressionPart{n.left, n.right}
	case membership:
		t.identity = "in"
		if n.array {
			t.identity = "any"
		}
		t.children = append([]expressionPart{n.value}, n.members...)
	case rowAggregate:
		t.identity, t.aggregate = "aggregate/"+n.fn, true
		if !n.star {
			t.children = []expressionPart{n.arg}
		}
	case rowNull:
		t.identity, t.children = "is null", []expressionPart{n.value}
	case rowNot:
		t.identity, t.children = "not", []expressionPart{n.term.part}
	case conjunction:
		t.identity, t.children = "and", predicateParts(n)
	case rowOr:
		t.identity, t.children = "or", predicateParts(n)
	default:
		return t, false
	}
	return t, true
}

func predicateParts(terms []Pred) []expressionPart {
	parts := make([]expressionPart, len(terms))
	for i, term := range terms {
		parts[i] = term.part
	}
	return parts
}

func sameExpression(a, b expressionPart) bool {
	switch left := a.(type) {
	case *fieldRef:
		right, ok := b.(*fieldRef)
		return ok && left != nil && right != nil && left.source == right.source &&
			left.name == right.name && sameSQLType(left.kind, right.kind)
	case *typedInput:
		right, ok := b.(*typedInput)
		return ok && left != nil && left == right
	}
	left, ok := expressionStructure(a)
	if !ok {
		return false
	}
	right, ok := expressionStructure(b)
	return ok && left.identity == right.identity && left.aggregate == right.aggregate &&
		slices.EqualFunc(left.children, right.children, sameExpression)
}

// repeatableExpression excludes clocks, session-dependent operations and
// volatile calls at any depth in an ordering or result-key proof.
func repeatableExpression(p expressionPart) bool {
	t, ok := expressionStructure(p)
	if !ok || t.stability != expressionImmutable {
		return false
	}
	for _, child := range t.children {
		if !repeatableExpression(child) {
			return false
		}
	}
	return true
}
