package tabula

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// PGType pairs a Go value type with an exact PostgreSQL catalog type.
// Its zero value cannot be used to declare a column or parameter.
type PGType[V any] struct {
	id       catalogType
	encode   func(V) any
	nullable bool
}

type catalogType struct {
	schema, name string
	goType       reflect.Type
}

// PostgreSQL identifiers are quoted one component at a time. Reject names
// the server would truncate, even when two such inputs would print differently.
func typedIdent(s string) bool {
	return s != "" && len(s) <= 63 && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

func typedQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// CatalogType declares a schema-qualified PostgreSQL type. Supply its encoding
// for a bound input, or nil when the driver already accepts V. Both names are
// exact catalog identifiers. No portion is interpreted as a SQL expression.
func CatalogType[V any](schema, name string, encode func(V) any) PGType[V] {
	if !typedIdent(schema) || !typedIdent(name) {
		panic("tabula.CatalogType: invalid schema or type name")
	}
	typ := reflect.TypeFor[V]()
	nullable := encode != nil
	switch typ.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		// A named scalar may still implement a custom encoding.
		nullable = nullable || typ.NumMethod() > 0
	default:
		nullable = nullable || typ != reflect.TypeFor[time.Time]()
	}
	return PGType[V]{id: catalogType{schema, name, typ}, encode: encode, nullable: nullable}
}

// Bool declares PostgreSQL boolean.
func Bool() PGType[bool] { return CatalogType[bool]("pg_catalog", "bool", nil) }

// Int4 declares PostgreSQL integer.
func Int4() PGType[int32] { return CatalogType[int32]("pg_catalog", "int4", nil) }

// Int8 declares PostgreSQL bigint.
func Int8() PGType[int64] { return CatalogType[int64]("pg_catalog", "int8", nil) }

// Text declares PostgreSQL text.
func Text() PGType[string] { return CatalogType[string]("pg_catalog", "text", nil) }

// Timestamptz declares PostgreSQL timestamp with time zone.
func Timestamptz() PGType[time.Time] { return CatalogType[time.Time]("pg_catalog", "timestamptz", nil) }

// Slot is the identity of a typed input. Reusing it in a statement reuses the
// parameter position. Two Input calls always allocate distinct inputs.
type Slot[V any] struct {
	input *typedInput
	kind  PGType[V]
}

type typedInput struct {
	name string
	kind catalogType
}

// Input declares one named input at definition time.
func Input[V any](name string, kind PGType[V]) Slot[V] {
	if !typedIdent(name) || kind.id.goType == nil {
		panic("tabula.Input: a named input and a PostgreSQL type are required")
	}
	return Slot[V]{input: &typedInput{name, kind.id}, kind: kind}
}

// Binding supplies the value of one Slot to an execution.
type Binding struct {
	input *typedInput
	value any
}

// BindValue pairs an input with a value of its declared Go type.
func BindValue[V any](slot Slot[V], value V) Binding {
	if slot.kind.encode != nil {
		return Binding{slot.input, slot.kind.encode(value)}
	}
	return Binding{slot.input, value}
}

// parameterLayout is built in emitted SQL order and frozen with a shape.
// A scope value, when needed, always occupies $1. The nil input is private
// to the scope slot and can never be supplied by a caller binding.
type parameterLayout struct {
	scoped    bool
	positions map[*typedInput]int
	names     map[string]*typedInput
	order     []*typedInput
}

func newParameterLayout(scoped bool) *parameterLayout {
	return &parameterLayout{scoped: scoped, positions: make(map[*typedInput]int), names: make(map[string]*typedInput)}
}

func (l *parameterLayout) parameter(input *typedInput) (string, error) {
	if input == nil {
		return "", fmt.Errorf("tabula: undeclared input")
	}
	if existing := l.names[input.name]; existing != nil && existing != input {
		return "", fmt.Errorf("tabula: distinct inputs share the name %q", input.name)
	}
	position, ok := l.positions[input]
	if !ok {
		position = len(l.order)
		if l.scoped {
			position++
		}
		if position >= 65535 {
			return "", fmt.Errorf("tabula: PostgreSQL parameter limit exceeded")
		}
		l.positions[input] = position
		l.names[input.name] = input
		l.order = append(l.order, input)
	}
	return "$" + strconv.Itoa(position+1), nil
}

func (l *parameterLayout) frozen() *parameterLayout {
	return &parameterLayout{scoped: l.scoped, positions: maps.Clone(l.positions), order: slices.Clone(l.order)}
}

func (l *parameterLayout) bind(scope any, bindings []Binding) ([]any, error) {
	count := len(l.order)
	if l.scoped {
		count++
	}
	args := make([]any, count)
	if l.scoped {
		args[0] = scope
	}
	seen := make([]bool, count)
	for _, b := range bindings {
		index, ok := l.positions[b.input]
		if !ok {
			return nil, fmt.Errorf("tabula: binding does not belong to this shape")
		}
		if seen[index] {
			return nil, fmt.Errorf("tabula: duplicate binding for %q", b.input.name)
		}
		seen[index] = true
		args[index] = b.value
	}
	for _, input := range l.order {
		if !seen[l.positions[input]] {
			return nil, fmt.Errorf("tabula: missing binding for %q", input.name)
		}
	}
	return args, nil
}
