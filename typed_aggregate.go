package tabula

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// aggregateCall retains one root. Modifiers never wrap scalar expressions or
// each other, so duplicate modifiers and nested aggregates fail at definition.
type aggregateCall struct {
	fn                            string
	args                          []scalarOperand
	result                        catalogType
	star, valid, legacy, distinct bool
	order                         []RowTerm
	filter                        *Pred
	err                           error
}

type aggregateRoot interface{ aggregateRoot() aggregateCall }

func (a aggregateCall) aggregateRoot() aggregateCall                { return a }
func (a aggregateCall) nullableAfterJoin(map[*occurrence]bool) bool { return a.fn != "count" }
func (a aggregateCall) expressionTraits() expressionTraits {
	t := expressionTraits{identity: "aggregate/" + a.fn + "/" + a.result.qualified() + "/" + strconv.FormatBool(a.star) + "/" + strconv.FormatBool(a.distinct), aggregate: true}
	// JSON's polymorphic conversion is catalog-STABLE. An unspecified input
	// order also cannot establish a reproducible result cursor.
	if a.fn == "json_agg" || a.fn == "jsonb_agg" || (a.fn == "string_agg" || a.fn == "array_agg") && len(a.order) == 0 {
		t.stability = expressionStable
	}
	for _, arg := range a.args {
		t.children = append(t.children, arg.part)
		t.identity += "/" + arg.kind.qualified()
	}
	for _, order := range a.order {
		t.children = append(t.children, order.orderValue().part)
		t.identity += "/order/" + strconv.FormatBool(order.desc) + "/" + strconv.FormatBool(order.nullsFirst())
	}
	if a.filter != nil {
		t.identity += "/filter"
		t.children = append(t.children, a.filter.part)
	}
	return t
}

// FilterWhere applies SQL FILTER to one aggregate, including existing CountOf,
// SumOf and AvgOf expressions. Filtering preserves the native empty-input result.
func FilterWhere[V any](aggregate Expr[V], condition Pred) Expr[V] {
	a := modifiedAggregate(aggregate.part)
	if a.filter != nil {
		a.err = fmt.Errorf("tabula: duplicate aggregate FILTER")
	}
	a.filter = &condition
	aggregate.part = a
	return aggregate
}

// DistinctInputs removes duplicate argument tuples before aggregation. It
// requires comparable arguments and does not accept CountAll or JSON inputs.
func DistinctInputs[V any](aggregate Expr[V]) Expr[V] {
	a := modifiedAggregate(aggregate.part)
	if a.distinct {
		a.err = fmt.Errorf("tabula: duplicate aggregate DISTINCT")
	}
	a.distinct = true
	aggregate.part = a
	return aggregate
}

// CountDistinct counts distinct non-NULL values using their native equality.
func CountDistinct[V any](value Expr[V]) Expr[int64] { return DistinctInputs(CountOf(value)) }

// AggregateOrder declares input order for string, array or JSON aggregation.
// With DistinctInputs each ordering expression must also be an argument.
func AggregateOrder[V any](aggregate Expr[V], first RowTerm, rest ...RowTerm) Expr[V] {
	a := modifiedAggregate(aggregate.part)
	if len(a.order) != 0 {
		a.err = fmt.Errorf("tabula: duplicate aggregate ORDER BY")
	}
	a.order = slices.Concat([]RowTerm{first}, rest)
	aggregate.part = a
	return aggregate
}

func modifiedAggregate(part expressionPart) aggregateCall {
	root, ok := part.(aggregateRoot)
	if !ok {
		return aggregateCall{err: fmt.Errorf("tabula: aggregate modifier requires a single aggregate root")}
	}
	a := root.aggregateRoot()
	a.legacy = false
	return a
}

// StringAgg concatenates non-NULL text values with the bound text delimiter.
// Empty or all-NULL input returns SQL NULL. Use AggregateOrder for input order.
func StringAgg[A, B ~string](value Expr[A], delimiter Expr[B]) Expr[string] {
	return newAggregate("string_agg", Text(), value.scalarOperand(), delimiter.scalarOperand())
}

// ArrayAgg collects non-NULL scalar elements using the declared array codec.
// Nullable elements require ArrayAggAs with an array-of-nullable result type.
func ArrayAgg[V any](element Expr[V]) Expr[[]V] {
	if element.kind.id.goType == nil || element.kind.id.element != nil {
		return Expr[[]V]{part: aggregateCall{err: fmt.Errorf("tabula: ArrayAgg needs a declared scalar element")}}
	}
	return ArrayAggAs(ArrayOf(element.kind), element)
}

// ArrayAggAs collects scalar elements into the exact declared array type.
// A nullable input requires a result codec preserving individual NULL elements.
// Empty input returns a NULL array, independently of a NULL array element.
func ArrayAggAs[R, V any](result PGType[R], element Expr[V]) Expr[R] {
	return newAggregate("array_agg", result, element.scalarOperand())
}

// JSONAgg collects typed values, including SQL NULL as JSON null, using the
// declared json or jsonb result codec. Empty input returns SQL NULL.
func JSONAgg[R, V any](result PGType[R], value Expr[V]) Expr[R] {
	fn := "json_agg"
	if result.id.name == "jsonb" {
		fn = "jsonb_agg"
	}
	return newAggregate(fn, result, value.scalarOperand())
}

// JSONBAgg is JSONAgg restricted to a declared pg_catalog.jsonb result.
func JSONBAgg[R, V any](result PGType[R], value Expr[V]) Expr[R] {
	return newAggregate("jsonb_agg", result, value.scalarOperand())
}

// BoolOr reports whether any non-NULL input is true. Empty or all-NULL input
// yields NULL. It accepts FilterWhere and DistinctInputs.
func BoolOr(value Pred) Expr[bool] { return newAggregate("bool_or", Bool(), value.scalarOperand()) }

// BoolAnd reports whether every non-NULL input is true, with BoolOr's NULL contract.
func BoolAnd(value Pred) Expr[bool] { return newAggregate("bool_and", Bool(), value.scalarOperand()) }

func newAggregate[R any](fn string, result PGType[R], args ...scalarOperand) Expr[R] {
	return Expr[R]{aggregateCall{fn: fn, args: args, result: result.id, valid: true}, result, true}
}

func (a aggregateCall) validate(outer map[*occurrence]bool) error {
	if a.err != nil {
		return a.err
	}
	if !a.valid || a.result.goType == nil {
		return fmt.Errorf("tabula: invalid field or PostgreSQL overload for %s", a.fn)
	}
	for _, arg := range a.args {
		if arg.kind.goType == nil {
			return fmt.Errorf("tabula: %s requires declared arguments", a.fn)
		}
		if err := checkRowExpr(arg.part, nil); err != nil {
			return err
		}
	}
	if a.distinct {
		if a.star {
			return fmt.Errorf("tabula: DISTINCT requires non-star aggregate arguments")
		}
		for _, arg := range a.args {
			if !resultComparable(arg.kind) {
				return fmt.Errorf("tabula: DISTINCT aggregate requires comparable arguments, got %s", arg.kind.qualified())
			}
		}
	}
	if len(a.order) > 0 && a.fn != "string_agg" && a.fn != "array_agg" && a.fn != "json_agg" && a.fn != "jsonb_agg" {
		return fmt.Errorf("tabula: aggregate ORDER BY requires an order-sensitive aggregate")
	}
	for _, term := range a.order {
		v := term.orderValue()
		if err := checkRowExpr(v.part, nil); err != nil {
			return err
		}
		if !resultComparable(v.kind) {
			return fmt.Errorf("tabula: aggregate order requires a comparable type")
		}
		if a.distinct && !slices.ContainsFunc(a.args, func(arg scalarOperand) bool { return sameExpression(arg.part, v.part) }) {
			return fmt.Errorf("tabula: DISTINCT aggregate ORDER BY must use its arguments")
		}
	}
	if a.filter != nil {
		if err := checkRowBool(*a.filter); err != nil {
			return err
		}
		if err := checkRowExpr(a.filter.part, nil); err != nil {
			return err
		}
	}
	return a.checkSignature(outer)
}

func (a aggregateCall) checkSignature(outer map[*occurrence]bool) error {
	valid := true
	switch a.fn {
	case "string_agg":
		valid = len(a.args) == 2 && sameSQLType(a.args[0].kind, Text().id) && sameSQLType(a.args[1].kind, Text().id)
	case "bool_or", "bool_and":
		valid = len(a.args) == 1 && sameSQLType(a.args[0].kind, Bool().id)
	case "array_agg":
		valid = len(a.args) == 1 && a.result.element != nil && sameSQLType(*a.result.element, a.args[0].kind) && a.result.element.goType == a.args[0].kind.goType && arrayAggregateElement(a.args[0].kind)
		if valid && a.args[0].nullableWith(outer) {
			valid = a.result.goType.Kind() == reflect.Slice && a.result.goType.Elem().Kind() == reflect.Pointer && a.result.goType.Elem().Elem() == a.args[0].kind.goType
			if !valid {
				return fmt.Errorf("tabula: array_agg nullable input requires an array-of-nullable result codec")
			}
		}
	case "json_agg", "jsonb_agg":
		valid = len(a.args) == 1 && a.result.schema == "pg_catalog" && a.result.name == strings.TrimSuffix(a.fn, "_agg") && aggregateJSONInput(a.args[0].kind)
	case "min", "max":
		if a.legacy {
			return nil
		}
		// PostgreSQL does not supply min/max for bool, jsonb or bytea.
		valid = len(a.args) == 1 && resultComparable(a.args[0].kind)
		if valid {
			switch a.args[0].kind.name {
			case "bool", "jsonb", "bytea", "uuid", "varchar", "cidr", "macaddr", "macaddr8":
				valid = false
			}
		}
	}
	if !valid {
		return fmt.Errorf("tabula: invalid PostgreSQL argument or result type for %s", a.fn)
	}
	return nil
}

func arrayAggregateElement(t catalogType) bool {
	if t.schema != "pg_catalog" || t.element != nil || t.goType == nil {
		return false
	}
	switch t.name {
	case "bool", "int2", "int4", "int8", "oid", "float4", "float8", "text", "varchar", "bpchar":
		return true
	}
	return false
}

func aggregateJSONInput(t catalogType) bool {
	if t.element != nil {
		return aggregateJSONInput(*t.element)
	}
	return resultComparable(t) || t.schema == "pg_catalog" && t.name == "json" && t.goType != nil
}

func (a aggregateCall) render(c *typedCompiler) (string, error) {
	if err := a.validate(nil); err != nil {
		return "", err
	}
	parts := make([]string, len(a.args))
	for i, arg := range a.args {
		part, err := arg.part.render(c)
		if err != nil {
			return "", err
		}
		parts[i] = part
	}
	args := strings.Join(parts, ", ")
	if a.star {
		args = "*"
	}
	if a.distinct {
		args = "DISTINCT " + args
	}
	if len(a.order) > 0 {
		order, err := (&rowOrdering{terms: a.order}).orderSQL(c)
		if err != nil {
			return "", err
		}
		args += order
	}
	query := "pg_catalog." + a.fn + "(" + args + ")"
	if a.filter != nil {
		filter, err := renderRowPredicate(c, *a.filter)
		if err != nil {
			return "", err
		}
		query += " FILTER (WHERE " + filter + ")"
	}
	if !a.legacy {
		c.requireNative(a.signature())
	}
	return query, nil
}

func (a aggregateCall) signature() string {
	args := make([]string, len(a.args))
	for i, arg := range a.args {
		args[i] = arg.kind.qualified()
	}
	switch a.fn {
	case "count":
		if !a.star {
			args = []string{`"any"`}
		}
	case "array_agg":
		args = []string{"anynonarray"}
	case "json_agg", "jsonb_agg":
		args = []string{"anyelement"}
	case "min", "max":
		if len(a.args) == 1 && a.args[0].kind.element != nil {
			args = []string{"anyarray"}
		}
	}
	return "pg_catalog." + a.fn + "(" + strings.Join(args, ",") + ")"
}

func checkAggregateNulls(p expressionPart, outer map[*occurrence]bool) error {
	if a, ok := p.(aggregateRoot); ok {
		return a.aggregateRoot().validate(outer)
	}
	t, ok := expressionStructure(p)
	if !ok {
		return fmt.Errorf("tabula: unsupported or undeclared row expression")
	}
	for _, child := range t.children {
		if err := checkAggregateNulls(child, outer); err != nil {
			return err
		}
	}
	return nil
}
