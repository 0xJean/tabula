package plan

import "github.com/0xJean/tabula"

type rowAggregate struct {
	fn     string
	arg    expressionPart
	star   bool
	valid  bool
	input  scalarOperand
	result catalogType
}

func (a rowAggregate) render(c *typedCompiler) (string, error) {
	return a.aggregateRoot().render(c)
}

func (a rowAggregate) aggregateRoot() aggregateCall {
	args := []scalarOperand(nil)
	if !a.star {
		args = []scalarOperand{a.input}
	}
	return aggregateCall{fn: a.fn, args: args, result: a.result, star: a.star, valid: a.valid, legacy: true}
}

func (a rowAggregate) expressionTraits() expressionTraits {
	return a.aggregateRoot().expressionTraits()
}
func (a rowAggregate) nullableAfterJoin(map[*occurrence]bool) bool { return a.fn != "count" }

// CountAll counts all rows, including those with NULL values.
func CountAll() Expr[int64] {
	return Expr[int64]{rowAggregate{fn: "count", star: true, valid: true, result: typeID(Int8())}, Int8(), false}
}

// CountOf counts non-null values of the input expression.
func CountOf[V any](f Expr[V]) Expr[int64] {
	return Expr[int64]{rowAggregate{fn: "count", arg: f.part, valid: f.part != nil, input: f.scalarOperand(), result: typeID(Int8())}, Int8(), false}
}

// MinOf computes the minimum of a field. An empty or all-NULL input yields NULL.
// PostgreSQL must provide the corresponding same-result-type min overload.
func MinOf[V any](f Expr[V]) Expr[V] {
	return Expr[V]{rowAggregate{fn: "min", arg: f.part, valid: f.part != nil, input: f.scalarOperand(), result: typeID(f.kind)}, f.kind, true}
}

// MaxOf computes the maximum of a field, with the same NULL contract as MinOf.
func MaxOf[V any](f Expr[V]) Expr[V] {
	return Expr[V]{rowAggregate{fn: "max", arg: f.part, valid: f.part != nil, input: f.scalarOperand(), result: typeID(f.kind)}, f.kind, true}
}

// SumOf sums a numeric field. Supply the exact PostgreSQL result type and
// its Go representation. For example, sum(int4) returns int8 and sum(int8)
// returns numeric. An empty or all-NULL group yields NULL.
func SumOf[V, R any](f Expr[V], result tabula.PGType[R]) Expr[R] {
	return numericAggregate("sum", f, result)
}

// AvgOf averages a numeric field. PostgreSQL returns numeric for integer
// and numeric inputs and float8 for floating-point inputs. The result is nullable.
func AvgOf[V, R any](f Expr[V], result tabula.PGType[R]) Expr[R] {
	return numericAggregate("avg", f, result)
}

func numericAggregate[V, R any](fn string, f Expr[V], result tabula.PGType[R]) Expr[R] {
	name := typeID(f.kind).Name
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
	valid := want != "" && typeID(f.kind).Schema == "pg_catalog" && typeID(result).Schema == "pg_catalog" && typeID(result).Name == want
	return Expr[R]{rowAggregate{fn: fn, arg: f.part, valid: valid, input: f.scalarOperand(), result: typeID(result)}, result, true}
}
