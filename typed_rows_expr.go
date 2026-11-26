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

type rowAggregate struct {
	fn    string
	arg   expressionPart
	star  bool
	valid bool
}

func (a rowAggregate) render(c *typedCompiler) (string, error) {
	if !a.valid {
		return "", fmt.Errorf("tabula: invalid field or PostgreSQL overload for %s", a.fn)
	}
	arg := "*"
	if !a.star {
		var err error
		if err := checkRowExpr(a.arg, nil); err != nil {
			return "", err
		}
		arg, err = a.arg.render(c)
		if err != nil {
			return "", err
		}
	}
	return "pg_catalog." + a.fn + "(" + arg + ")", nil
}

// CountAll counts all rows, including those with NULL values.
func CountAll() Expr[int64] {
	return Expr[int64]{rowAggregate{fn: "count", star: true, valid: true}, Int8(), false}
}

// CountOf counts non-null values of the input expression.
func CountOf[V any](f Expr[V]) Expr[int64] {
	return Expr[int64]{rowAggregate{fn: "count", arg: f.part, valid: f.part != nil}, Int8(), false}
}

// MinOf computes the minimum of a field. An empty or all-NULL input yields NULL.
// PostgreSQL must provide the corresponding same-result-type min overload.
func MinOf[V any](f Expr[V]) Expr[V] {
	return Expr[V]{rowAggregate{fn: "min", arg: f.part, valid: f.part != nil}, f.kind, true}
}

// MaxOf computes the maximum of a field, with the same NULL contract as MinOf.
func MaxOf[V any](f Expr[V]) Expr[V] {
	return Expr[V]{rowAggregate{fn: "max", arg: f.part, valid: f.part != nil}, f.kind, true}
}

// SumOf sums a numeric field. Supply the exact PostgreSQL result type and
// its Go representation. For example, sum(int4) returns int8 and sum(int8)
// returns numeric. An empty or all-NULL group yields NULL.
func SumOf[V, R any](f Expr[V], result PGType[R]) Expr[R] {
	return numericAggregate("sum", f, result)
}

// AvgOf averages a numeric field. PostgreSQL returns numeric for integer
// and numeric inputs and float8 for floating-point inputs. The result is nullable.
func AvgOf[V, R any](f Expr[V], result PGType[R]) Expr[R] {
	return numericAggregate("avg", f, result)
}

func numericAggregate[V, R any](fn string, f Expr[V], result PGType[R]) Expr[R] {
	name := f.kind.id.name
	var want string
	switch name {
	case "int2", "int4":
		want = "int8"
		if fn == "avg" {
			want = "numeric"
		}
	case "int8", "numeric":
		want = "numeric"
	case "float4", "float8":
		want = name
		if fn == "avg" {
			want = "float8"
		}
	case "interval":
		want = "interval"
	}
	valid := want != "" && f.kind.id.schema == "pg_catalog" && result.id.schema == "pg_catalog" && result.id.name == want
	return Expr[R]{rowAggregate{fn: fn, arg: f.part, valid: valid}, result, true}
}

// Visit free fields and complete aggregate nodes separately so a grouped
// projection cannot read an ungrouped field through a comparison.
func checkRowExpr(p expressionPart, visit func(expressionPart) error) error {
	switch v := p.(type) {
	case comparison:
		if err := checkRowExpr(v.left, visit); err != nil {
			return err
		}
		return checkRowExpr(v.right, visit)
	case conjunction:
		return checkRowTerms(v, visit)
	case rowOr:
		return checkRowTerms(v, visit)
	case rowNot:
		return checkRowTerms([]Pred{v.term}, visit)
	case rowNull:
		return checkRowExpr(v.value, visit)
	case rowAggregate:
		if visit == nil {
			return fmt.Errorf("tabula: aggregate outside result projection")
		}
	case *fieldRef, *typedInput:
	default:
		return fmt.Errorf("tabula: unsupported or undeclared row expression")
	}
	if visit != nil {
		return visit(p)
	}
	return nil
}

func checkRowTerms(terms []Pred, visit func(expressionPart) error) error {
	for _, p := range terms {
		if err := checkRowBool(p); err != nil {
			return err
		}
		if err := checkRowExpr(p.part, visit); err != nil {
			return err
		}
	}
	return nil
}
