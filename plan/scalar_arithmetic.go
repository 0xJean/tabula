package plan

import "github.com/0xJean/tabula"

// Plus adds operands only when PostgreSQL returns their declared type.
func Plus[V any](a, b Expr[V]) Expr[V] { return PlusAs(a.kind, a, b) }

// Minus subtracts operands only when PostgreSQL returns their declared type.
func Minus[V any](a, b Expr[V]) Expr[V] { return MinusAs(a.kind, a, b) }

// Times multiplies operands only when PostgreSQL returns their declared type.
func Times[V any](a, b Expr[V]) Expr[V] { return TimesAs(a.kind, a, b) }

// Divide divides operands with native PostgreSQL semantics, including integer
// division, division-by-zero errors and overflow.
func Divide[V any](a, b Expr[V]) Expr[V] { return DivideAs(a.kind, a, b) }

// PlusAs selects a checked native addition overload and its exact result type.
// Native date + time and time + date return timestamp without time zone.
func PlusAs[R, A, B any](result tabula.PGType[R], a Expr[A], b Expr[B]) Expr[R] {
	return arithmetic(result, "+", a.scalarOperand(), b.scalarOperand())
}

// MinusAs selects a checked native subtraction overload and exact result type.
func MinusAs[R, A, B any](result tabula.PGType[R], a Expr[A], b Expr[B]) Expr[R] {
	return arithmetic(result, "-", a.scalarOperand(), b.scalarOperand())
}

// TimesAs selects a checked native multiplication overload and exact result type.
func TimesAs[R, A, B any](result tabula.PGType[R], a Expr[A], b Expr[B]) Expr[R] {
	return arithmetic(result, "*", a.scalarOperand(), b.scalarOperand())
}

// DivideAs selects a checked native division overload and exact result type.
func DivideAs[R, A, B any](result tabula.PGType[R], a Expr[A], b Expr[B]) Expr[R] {
	return arithmetic(result, "/", a.scalarOperand(), b.scalarOperand())
}

func arithmetic[R any](result tabula.PGType[R], op string, a, b scalarOperand) Expr[R] {
	n := scalarNode{syntax: scalarOperator, name: op, args: []scalarOperand{a, b}}
	want := arithmeticResult(op, a.kind, b.kind)
	if want == "" || !nativeType(typeID(result), want) {
		n.err = scalarError(op, "a native overload returning "+typeID(result).Qualified()+" (use Cast for other numeric combinations)", n.args)
	}
	if nativeType(a.kind, "timestamptz") && nativeType(b.kind, "interval") ||
		nativeType(a.kind, "interval") && nativeType(b.kind, "timestamptz") {
		n.stability = expressionStable
	}
	return scalarResult(result, n)
}

func arithmeticResult(op string, a, b catalogType) string {
	if !reviewedType(a) || !reviewedType(b) || a.Element != nil || b.Element != nil {
		return ""
	}
	if nativeType(a, "int2", "int4", "int8") && nativeType(b, "int2", "int4", "int8") {
		return max(a.Name, b.Name)
	}
	if sameSQLType(a, b) && nativeType(a, "numeric", "float4", "float8") {
		return a.Name
	}
	switch op {
	case "+":
		switch a.Name + "/" + b.Name {
		case "date/int4", "int4/date":
			return "date"
		case "timestamp/interval", "interval/timestamp", "date/interval", "interval/date":
			return "timestamp"
		case "date/time", "time/date":
			return "timestamp"
		case "timestamptz/interval", "interval/timestamptz":
			return "timestamptz"
		case "interval/interval":
			return "interval"
		case "time/interval", "interval/time":
			return "time"
		case "timetz/interval", "interval/timetz":
			return "timetz"
		}
	case "-":
		switch a.Name + "/" + b.Name {
		case "date/int4":
			return "date"
		case "date/date":
			return "int4"
		case "timestamp/timestamp", "timestamptz/timestamptz", "time/time", "interval/interval":
			return "interval"
		case "timestamp/interval", "date/interval":
			return "timestamp"
		case "timestamptz/interval", "time/interval", "timetz/interval":
			return a.Name
		}
	case "*":
		if a.Name == "interval" && b.Name == "float8" || a.Name == "float8" && b.Name == "interval" {
			return "interval"
		}
	case "/":
		if a.Name == "interval" && b.Name == "float8" {
			return "interval"
		}
	}
	return ""
}
