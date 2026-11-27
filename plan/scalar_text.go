package plan

import (
	"strings"

	"github.com/0xJean/tabula"
)

func textOperand(v scalarOperand) scalarOperand {
	if nativeType(v.kind, "text") {
		return v
	}
	n := scalarNode{syntax: scalarCast, name: "text coercion", result: typeID(Text()), args: []scalarOperand{v}}
	if !nativeType(v.kind, "varchar", "bpchar") {
		n.err = scalarError(n.name, "pg_catalog.text, varchar or bpchar", n.args)
	}
	return scalarOperand{n, n.result, v.nullable}
}

func checkedCall[V any](result tabula.PGType[V], name string, nulls scalarNullRule, stability expressionStability, types []string, args ...scalarOperand) Expr[V] {
	n := scalarNode{name: name, nulls: nulls, stability: stability, args: args,
		functions: []string{"pg_catalog." + name + "(" + strings.Join(types, ",") + ")"}}
	if len(types) != len(args) {
		n.err = scalarError(name, "its declared argument count", args)
	} else {
		for i, arg := range args {
			if !nativeType(arg.kind, types[i]) {
				n.err = scalarError(name, "("+strings.Join(types, ",")+")", args)
			}
		}
	}
	return scalarResult(result, n)
}

func textUnary[V ~string](name string, value Expr[V]) Expr[string] {
	return checkedCall(Text(), name, nullStrict, expressionImmutable, []string{"text"}, textOperand(value.scalarOperand()))
}

// Lower returns PostgreSQL text converted to lower case under its collation.
func Lower[V ~string](value Expr[V]) Expr[string] { return textUnary("lower", value) }

// Upper returns PostgreSQL text converted to upper case under its collation.
func Upper[V ~string](value Expr[V]) Expr[string] { return textUnary("upper", value) }

// Trim removes spaces from both ends, using PostgreSQL's btrim(text).
func Trim[V ~string](value Expr[V]) Expr[string] { return BTrim(value) }

// BTrim trims spaces, or the supplied set of characters, from both ends.
func BTrim[V ~string](value Expr[V], characters ...Expr[string]) Expr[string] {
	args := []scalarOperand{textOperand(value.scalarOperand())}
	types := []string{"text"}
	for _, v := range characters {
		args, types = append(args, textOperand(v.scalarOperand())), append(types, "text")
	}
	if len(characters) > 1 {
		types = nil
	}
	return checkedCall(Text(), "btrim", nullStrict, expressionImmutable, types, args...)
}

// Length counts characters, not bytes, and returns PostgreSQL int4.
func Length[V ~string](value Expr[V]) Expr[int32] {
	return checkedCall(Int4(), "length", nullStrict, expressionImmutable, []string{"text"}, textOperand(value.scalarOperand()))
}

// Substr selects characters with one-based int4 positions and an optional count.
func Substr[V ~string](value Expr[V], start Expr[int32], count ...Expr[int32]) Expr[string] {
	args := []scalarOperand{textOperand(value.scalarOperand()), start.scalarOperand()}
	types := []string{"text", "int4"}
	for _, v := range count {
		args, types = append(args, v.scalarOperand()), append(types, "int4")
	}
	if len(count) > 1 {
		types = nil
	}
	return checkedCall(Text(), "substr", nullStrict, expressionImmutable, types, args...)
}

// StartsWith tests a text prefix, preserving SQL NULL.
func StartsWith[A, B ~string](value Expr[A], prefix Expr[B]) Pred {
	return checkedCall(Bool(), "starts_with", nullStrict, expressionImmutable, []string{"text", "text"},
		textOperand(value.scalarOperand()), textOperand(prefix.scalarOperand()))
}

// RegexpReplace replaces matches using the PostgreSQL 14 three/four-argument
// overload. Pattern, replacement and optional flags are bound expressions.
func RegexpReplace[V ~string](value Expr[V], pattern, replacement Expr[string], flags ...Expr[string]) Expr[string] {
	args := []scalarOperand{textOperand(value.scalarOperand()), textOperand(pattern.scalarOperand()), textOperand(replacement.scalarOperand())}
	types := []string{"text", "text", "text"}
	for _, flag := range flags {
		args, types = append(args, textOperand(flag.scalarOperand())), append(types, "text")
	}
	if len(flags) > 1 {
		types = nil
	}
	return checkedCall(Text(), "regexp_replace", nullStrict, expressionImmutable, types, args...)
}

func regex[A, B ~string](op string, value Expr[A], pattern Expr[B]) Pred {
	return scalarResult(Bool(), scalarNode{syntax: scalarOperator, name: op,
		args: []scalarOperand{textOperand(value.scalarOperand()), textOperand(pattern.scalarOperand())}})
}

// RegexpMatch applies the native case-sensitive ~ operator.
func RegexpMatch[A, B ~string](value Expr[A], pattern Expr[B]) Pred {
	return regex("~", value, pattern)
}

// RegexpIMatch applies the native case-insensitive ~* operator.
func RegexpIMatch[A, B ~string](value Expr[A], pattern Expr[B]) Pred {
	return regex("~*", value, pattern)
}

// RegexpNotMatch applies the native case-sensitive !~ operator.
func RegexpNotMatch[A, B ~string](value Expr[A], pattern Expr[B]) Pred {
	return regex("!~", value, pattern)
}

// RegexpNotIMatch applies the native case-insensitive !~* operator.
func RegexpNotIMatch[A, B ~string](value Expr[A], pattern Expr[B]) Pred {
	return regex("!~*", value, pattern)
}

// TextCat concatenates two text operands strictly: either SQL NULL yields NULL.
func TextCat[A, B ~string](a Expr[A], b Expr[B]) Expr[string] {
	return scalarResult(Text(), scalarNode{syntax: scalarOperator, name: "||",
		args: []scalarOperand{textOperand(a.scalarOperand()), textOperand(b.scalarOperand())}})
}

// ByteaCat concatenates byte strings, preserving SQL NULL.
func ByteaCat(a, b Expr[[]byte]) Expr[[]byte] {
	return sameTypeOperator("||", tabula.CatalogType[[]byte]("pg_catalog", "bytea", nil), a, b, nullStrict)
}

// Concat converts reviewed built-in values to text and skips SQL NULL values.
// Its formatting can depend on session settings, so it is not a cursor key.
func Concat(first Scalar, rest ...Scalar) Expr[string] {
	args := scalarArgs(append([]Scalar{first}, rest...)...)
	n := scalarNode{name: "concat", args: args, nulls: nullNever, stability: expressionStable,
		functions: []string{`pg_catalog.concat("any")`}}
	for _, a := range args {
		if !reviewedType(a.kind) || len(args) > 100 {
			n.err = scalarError(n.name, "1 to 100 reviewed built-in arguments", args)
		}
	}
	return scalarResult(Text(), n)
}

func sameTypeOperator[V any](op string, result tabula.PGType[V], a, b Expr[V], nulls scalarNullRule) Expr[V] {
	n := scalarNode{syntax: scalarOperator, name: op, args: scalarArgs(a, b), nulls: nulls}
	if !sameSQLType(typeID(result), typeID(a.kind)) || !sameSQLType(typeID(result), typeID(b.kind)) || !reviewedType(typeID(result)) {
		n.err = scalarError(op, "matching "+typeID(result).Qualified()+" operands", n.args)
	}
	return scalarResult(result, n)
}
