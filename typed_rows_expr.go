package tabula

import (
	"fmt"
	"slices"
	"strings"
)

// Ne compares two expressions for inequality.
func Ne[V any](a, b Expr[V]) Pred { return typedComparison("<>", a, b, a.nullable || b.nullable) }

// Lt compares two expressions using PostgreSQL less-than semantics.
func Lt[V any](a, b Expr[V]) Pred { return typedComparison("<", a, b, a.nullable || b.nullable) }

// Le compares two expressions using PostgreSQL less-than-or-equal semantics.
func Le[V any](a, b Expr[V]) Pred { return typedComparison("<=", a, b, a.nullable || b.nullable) }

// Gt compares two expressions using PostgreSQL greater-than semantics.
func Gt[V any](a, b Expr[V]) Pred { return typedComparison(">", a, b, a.nullable || b.nullable) }

// Ge compares two expressions using PostgreSQL greater-than-or-equal semantics.
func Ge[V any](a, b Expr[V]) Pred { return typedComparison(">=", a, b, a.nullable || b.nullable) }

type rowOr []Pred

// Or combines predicates in declaration order with SQL three-valued logic.
func Or(first, second Pred, rest ...Pred) Pred {
	terms := slices.Concat([]Pred{first, second}, rest)
	return Pred{rowOr(terms), Bool(), slices.ContainsFunc(terms, func(p Pred) bool { return p.nullable })}
}

func (terms rowOr) render(c *typedCompiler) (string, error) {
	parts := make([]string, len(terms))
	for i, p := range terms {
		s, err := renderRowPredicate(c, p)
		if err != nil {
			return "", err
		}
		parts[i] = s
	}
	return "(" + strings.Join(parts, " OR ") + ")", nil
}

type rowNot struct{ term Pred }

// Not negates a PostgreSQL boolean, preserving SQL NULL.
func Not(p Pred) Pred { return Pred{rowNot{p}, Bool(), p.nullable} }

func (v rowNot) render(c *typedCompiler) (string, error) {
	part, err := renderRowPredicate(c, v.term)
	return "(NOT " + part + ")", err
}

func checkRowBool(p Pred) error {
	if p.part == nil || !sameSQLType(p.kind.id, Bool().id) {
		return fmt.Errorf("tabula: predicate requires a declared pg_catalog.bool expression")
	}
	return nil
}

func renderRowPredicate(c *typedCompiler, p Pred) (string, error) {
	if err := checkRowBool(p); err != nil {
		return "", err
	}
	return p.part.render(c)
}

type rowNull struct{ value expressionPart }

// IsNull tests SQL NULL and always returns a non-null boolean.
func IsNull[V any](v Expr[V]) Pred { return Pred{rowNull{v.part}, Bool(), false} }

func (v rowNull) render(c *typedCompiler) (string, error) {
	if v.value == nil {
		return "", fmt.Errorf("tabula: IS NULL requires an expression")
	}
	part, err := v.value.render(c)
	return "(" + part + " IS NULL)", err
}

// checkRowExpr traverses non-aggregate children and treats an aggregate as a
// result boundary. Its arguments are checked separately to forbid nesting.
func checkRowExpr(p expressionPart, visit func(expressionPart) error) error {
	return checkPhaseExpr(p, visit, false)
}

// checkResultExpr admits the final window phase in projections and ordering.
// A query subexpression supplies its own lexical/phase boundary.
func checkResultExpr(p expressionPart, visit func(expressionPart) error) error {
	return checkPhaseExpr(p, visit, true)
}

func checkPhaseExpr(p expressionPart, visit func(expressionPart) error, windows bool) error {
	switch n := p.(type) {
	case conjunction:
		for _, term := range n {
			if err := checkRowBool(term); err != nil {
				return err
			}
		}
	case rowOr:
		for _, term := range n {
			if err := checkRowBool(term); err != nil {
				return err
			}
		}
	case rowNot:
		if err := checkRowBool(n.term); err != nil {
			return err
		}
	}
	traits, ok := expressionStructure(p)
	if !ok {
		return fmt.Errorf("tabula: unsupported or undeclared row expression")
	}
	if traits.aggregate && visit == nil {
		return fmt.Errorf("tabula: aggregate outside result projection")
	}
	if traits.window && !windows {
		return fmt.Errorf("tabula: window outside result projection or final ordering")
	}
	if visit != nil {
		if err := visit(p); err != nil {
			return err
		}
	}
	for _, child := range traits.children {
		next := visit
		if traits.aggregate {
			next = nil
		}
		if err := checkPhaseExpr(child, next, windows && !traits.aggregate && !traits.window); err != nil {
			return err
		}
	}
	return nil
}
