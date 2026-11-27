package plan

import (
	"github.com/0xJean/tabula"

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
	t := expressionTraits{identity: "aggregate/" + a.fn + "/" + a.result.Qualified() + "/" + strconv.FormatBool(a.star) + "/" + strconv.FormatBool(a.distinct), aggregate: true}
	// JSON's polymorphic conversion is catalog-STABLE. An unspecified input
	// order also cannot establish a reproducible result cursor.
	if a.fn == "json_agg" || a.fn == "jsonb_agg" || (a.fn == "string_agg" || a.fn == "array_agg") && len(a.order) == 0 {
		t.stability = expressionStable
	}
	for _, arg := range a.args {
		t.children = append(t.children, arg.part)
		t.identity += "/" + arg.kind.Qualified()
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
	if typeID(element.kind).GoType == nil || typeID(element.kind).Element != nil {
		return Expr[[]V]{part: aggregateCall{err: fmt.Errorf("tabula: ArrayAgg needs a declared scalar element")}}
	}
	return ArrayAggAs(ArrayOf(element.kind), element)
}

// ArrayAggAs collects scalar elements into the exact declared array type.
// A nullable input requires a result codec preserving individual NULL elements.
// Empty input returns a NULL array, independently of a NULL array element.
func ArrayAggAs[R, V any](result tabula.PGType[R], element Expr[V]) Expr[R] {
	return newAggregate("array_agg", result, element.scalarOperand())
}

// JSONAgg collects typed values, including SQL NULL as JSON null, using the
// declared json or jsonb result codec. Empty input returns SQL NULL.
func JSONAgg[R, V any](result tabula.PGType[R], value Expr[V]) Expr[R] {
	fn := "json_agg"
	if typeID(result).Name == "jsonb" {
		fn = "jsonb_agg"
	}
	return newAggregate(fn, result, value.scalarOperand())
}

// JSONBAgg is JSONAgg restricted to a declared pg_catalog.jsonb result.
func JSONBAgg[R, V any](result tabula.PGType[R], value Expr[V]) Expr[R] {
	return newAggregate("jsonb_agg", result, value.scalarOperand())
}

// BoolOr reports whether any non-NULL input is true. Empty or all-NULL input
// yields NULL. It accepts FilterWhere and DistinctInputs.
func BoolOr(value Pred) Expr[bool] { return newAggregate("bool_or", Bool(), value.scalarOperand()) }

// BoolAnd reports whether every non-NULL input is true, with BoolOr's NULL contract.
func BoolAnd(value Pred) Expr[bool] { return newAggregate("bool_and", Bool(), value.scalarOperand()) }

func newAggregate[R any](fn string, result tabula.PGType[R], args ...scalarOperand) Expr[R] {
	return Expr[R]{aggregateCall{fn: fn, args: args, result: typeID(result), valid: true}, result, true}
}

func (a aggregateCall) validate(outer map[*occurrence]bool) error {
	return a.validatePhase(outer, false)
}

func (a aggregateCall) validatePhase(outer map[*occurrence]bool, window bool) error {
	if a.err != nil {
		return a.err
	}
	if !a.valid || a.result.GoType == nil {
		return fmt.Errorf("tabula: invalid field or PostgreSQL overload for %s", a.fn)
	}
	if window && (a.distinct || len(a.order) != 0) {
		return fmt.Errorf("tabula: window aggregates cannot use DISTINCT or aggregate-local ORDER BY")
	}
	var argumentVisitor func(expressionPart) error
	if window {
		argumentVisitor = func(expressionPart) error { return nil }
	}
	for _, arg := range a.args {
		if arg.kind.GoType == nil {
			return fmt.Errorf("tabula: %s requires declared arguments", a.fn)
		}
		if err := checkRowExpr(arg.part, argumentVisitor); err != nil {
			return err
		}
	}
	if a.distinct {
		if a.star {
			return fmt.Errorf("tabula: DISTINCT requires non-star aggregate arguments")
		}
		for _, arg := range a.args {
			if !resultComparable(arg.kind) {
				return fmt.Errorf("tabula: DISTINCT aggregate requires comparable arguments, got %s", arg.kind.Qualified())
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
		if err := checkRowExpr(a.filter.part, argumentVisitor); err != nil {
			return err
		}
	}
	return a.checkSignature(outer)
}

func (a aggregateCall) checkSignature(outer map[*occurrence]bool) error {
	valid := true
	switch a.fn {
	case "string_agg":
		valid = len(a.args) == 2 && sameSQLType(a.args[0].kind, typeID(Text())) && sameSQLType(a.args[1].kind, typeID(Text()))
	case "bool_or", "bool_and":
		valid = len(a.args) == 1 && sameSQLType(a.args[0].kind, typeID(Bool()))
	case "array_agg":
		valid = len(a.args) == 1 && a.result.Element != nil && sameSQLType(*a.result.Element, a.args[0].kind) && a.result.Element.GoType == a.args[0].kind.GoType && arrayAggregateElement(a.args[0].kind)
		if valid && a.args[0].nullableWith(outer) {
			valid = a.result.GoType.Kind() == reflect.Slice && a.result.GoType.Elem().Kind() == reflect.Pointer && a.result.GoType.Elem().Elem() == a.args[0].kind.GoType
			if !valid {
				return fmt.Errorf("tabula: array_agg nullable input requires an array-of-nullable result codec")
			}
		}
	case "json_agg", "jsonb_agg":
		valid = len(a.args) == 1 && a.result.Schema == "pg_catalog" && a.result.Name == strings.TrimSuffix(a.fn, "_agg") && aggregateJSONInput(a.args[0].kind)
	case "min", "max":
		if a.legacy {
			return nil
		}
		// PostgreSQL does not supply min/max for bool, jsonb or bytea.
		valid = len(a.args) == 1 && resultComparable(a.args[0].kind)
		if valid {
			switch a.args[0].kind.Name {
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
	if t.Schema != "pg_catalog" || t.Element != nil || t.GoType == nil {
		return false
	}
	switch t.Name {
	case "bool", "int2", "int4", "int8", "oid", "float4", "float8", "text", "varchar", "bpchar":
		return true
	}
	return false
}

func aggregateJSONInput(t catalogType) bool {
	if t.Element != nil {
		return aggregateJSONInput(*t.Element)
	}
	return resultComparable(t) || t.Schema == "pg_catalog" && t.Name == "json" && t.GoType != nil
}

func (a aggregateCall) render(c *typedCompiler) (string, error) {
	return a.renderCall(c, false)
}

func (a aggregateCall) renderCall(c *typedCompiler, window bool) (string, error) {
	if err := a.validatePhase(nil, window); err != nil {
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
	if a.legacy {
		c.legacyAggregates = append(c.legacyAggregates, a)
	} else {
		c.requireNative(a.signature())
	}
	return query, nil
}

// Old MIN/MAX declarations also accept catalog types whose effective
// overload can only be resolved by PostgreSQL. Record a known native
// overload when a legacy aggregate is composed into a new expression.
func (a aggregateCall) knownLegacySignature() string {
	if a.fn != "min" && a.fn != "max" {
		return a.signature()
	}
	t := a.args[0].kind
	if t.Element != nil || nativeType(t, "bpchar", "date", "float4", "float8", "inet", "int2", "int4", "int8",
		"interval", "money", "numeric", "oid", "pg_lsn", "text", "tid", "time", "timestamp", "timestamptz", "timetz", "xid8") {
		return a.signature()
	}
	return ""
}

func (a aggregateCall) signature() string {
	args := make([]string, len(a.args))
	for i, arg := range a.args {
		args[i] = arg.kind.Qualified()
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
		if len(a.args) == 1 && a.args[0].kind.Element != nil {
			args = []string{"anyarray"}
		}
	}
	return "pg_catalog." + a.fn + "(" + strings.Join(args, ",") + ")"
}

func checkAggregateNulls(p expressionPart, outer map[*occurrence]bool) error {
	if w, ok := p.(windowExpression); ok {
		if err := w.validate(outer); err != nil {
			return err
		}
	}
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
