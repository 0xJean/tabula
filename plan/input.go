package plan

import (
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/0xJean/tabula"
)

// Slot is the identity of a typed input. Reusing it in a statement reuses the
// parameter position. Two Input calls always allocate distinct inputs.
type Slot[V any] struct {
	input *typedInput
	kind  tabula.PGType[V]
}

type typedInput struct {
	name   string
	kind   catalogType
	secret bool
	fixed  *int64
}

// Input declares one named input at definition time.
func Input[V any](name string, kind tabula.PGType[V]) Slot[V] {
	id := typeID(kind)
	if !typedIdent(name) || id.GoType == nil {
		panic("plan.Input: a named input and a PostgreSQL type are required")
	}
	return Slot[V]{input: &typedInput{name: name, kind: id}, kind: kind}
}

// Binding supplies the canonical value of one Slot. It can be reused across
// statements and sessions. Byte and array inputs snapshot their slice
// containers. Custom encoders own the stability of other returned values.
type Binding struct {
	input *typedInput
	value any
	err   error
}

func bindCatalogValue(kind catalogType, input *typedInput, value any) Binding {
	encoded, err := kind.EncodeValue(value)
	if err != nil {
		return Binding{input: input, err: err}
	}
	return Binding{input: input, value: encoded}
}

// BindValue pairs an input with a value of its declared Go type, running its
// encoder once. An encoding error is reported before a statement uses it.
func BindValue[V any](slot Slot[V], value V) Binding {
	return bindCatalogValue(typeID(slot.kind), slot.input, value)
}

// parameterLayout is built in emitted SQL order and frozen with a shape.
// A scope value, when needed, always occupies $1. The nil input is private
// to the scope slot and can never be supplied by a caller binding.
type parameterLayout struct {
	scoped    bool
	scopeType catalogType
	positions map[*typedInput]int
	names     map[string]*typedInput
	order     []*typedInput
	graph     bool
	limits    map[*typedInput]int64
}

func newParameterLayout(scoped bool) *parameterLayout {
	return &parameterLayout{scoped: scoped, positions: make(map[*typedInput]int), names: make(map[string]*typedInput)}
}

func (l *parameterLayout) parameter(input *typedInput) (string, error) {
	if input == nil {
		return "", fmt.Errorf("tabula: undeclared input")
	}
	if existing := l.names[input.name]; !l.graph && existing != nil && existing != input {
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
	return &parameterLayout{scoped: l.scoped, scopeType: l.scopeType, positions: maps.Clone(l.positions), order: slices.Clone(l.order), graph: l.graph, limits: maps.Clone(l.limits)}
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
		if b.err != nil {
			return nil, fmt.Errorf("tabula: encode input %q: %w", b.input.name, b.err)
		}
		seen[index] = true
		args[index] = b.input.kind.Snapshot(b.value)
	}
	for _, input := range l.order {
		if input.fixed != nil {
			args[l.positions[input]] = *input.fixed
			seen[l.positions[input]] = true
		}
		if !seen[l.positions[input]] {
			return nil, fmt.Errorf("tabula: missing binding for %q", input.name)
		}
	}
	for input, ceiling := range l.limits {
		value, ok := args[l.positions[input]].(int64)
		if !ok || value < 0 || value > ceiling {
			return nil, fmt.Errorf("tabula: limit %q must be between zero and %d", input.name, ceiling)
		}
	}
	return args, nil
}
