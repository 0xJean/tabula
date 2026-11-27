package plan

import (
	"fmt"
	"slices"
	"strings"

	"github.com/0xJean/tabula"
)

// RoutineCardinality is the number of results a function can return.
type RoutineCardinality uint8

const (
	// RoutineOne declares an ordinary function returning one value, possibly NULL.
	RoutineOne RoutineCardinality = iota + 1
	// RoutineSet declares a function returning SETOF its result type.
	RoutineSet
)

// RoutineSpec declares a scalar or set-returning PostgreSQL function.
// ReadOnly is an explicit assertion about the reviewed body. The default
// requires write authorization. Nullable describes individual result values.
type RoutineSpec[V any] struct {
	Schema, Name                        string
	Result                              tabula.PGType[V]
	Cardinality                         RoutineCardinality
	Nullable, ReadOnly, SecurityDefiner bool
}

// RoutineParameter declares one input in PostgreSQL signature order.
type RoutineParameter struct {
	name string
	kind catalogType
}

// Parameter names a formal parameter and its exact PostgreSQL type.
func Parameter[V any](name string, kind tabula.PGType[V]) RoutineParameter {
	return RoutineParameter{name, typeID(kind)}
}

// Routine is an immutable, schema-qualified function signature.
type Routine[V any] struct {
	spec        RoutineSpec[V]
	params      []RoutineParameter
	requirement tabula.RoutineRequirement
}

// DeclareRoutine freezes a function signature at startup. Supply the parameters
// in catalog order. Use AdvisoryXactLock for built-in advisory operations.
func DeclareRoutine[V any](spec RoutineSpec[V], params ...RoutineParameter) *Routine[V] {
	resID := typeID(spec.Result)
	if !typedIdent(spec.Schema) || !typedIdent(spec.Name) || resID.GoType == nil {
		panic("plan.DeclareRoutine: a qualified name and a result type are required")
	}
	if spec.Cardinality != RoutineOne && spec.Cardinality != RoutineSet {
		panic("plan.DeclareRoutine: specify RoutineOne or RoutineSet")
	}
	if spec.Schema == "pg_catalog" && (strings.HasPrefix(spec.Name, "pg_advisory_") || strings.HasPrefix(spec.Name, "pg_try_advisory_")) {
		panic("plan.DeclareRoutine: built-in advisory locks require a transaction lock declaration")
	}
	names := make(map[string]bool, len(params))
	types := make([]string, len(params))
	for i, p := range params {
		if !typedIdent(p.name) || names[p.name] || p.kind.GoType == nil {
			panic("plan.DeclareRoutine: invalid or duplicate parameter")
		}
		names[p.name] = true
		types[i] = p.kind.Qualified()
	}
	name := physicalName{Schema: spec.Schema, Name: spec.Name}
	return &Routine[V]{spec: spec, params: slices.Clone(params), requirement: tabula.RoutineRequirement{
		Signature: name.SQL() + "(" + strings.Join(types, ", ") + ")", ResultType: resID.Qualified(),
		ReturnsSet: spec.Cardinality == RoutineSet, ReadOnly: spec.ReadOnly, SecurityDefiner: spec.SecurityDefiner,
	}}
}

// Formal is a typed argument position owned by one declared routine.
type Formal[V any] struct{ param *RoutineParameter }

// FormalOf resolves a named parameter on this exact signature.
func FormalOf[R, V any](r *Routine[R], name string, kind tabula.PGType[V]) Formal[V] {
	id := typeID(kind)
	if r != nil {
		for i, p := range r.params {
			if name == p.name && sameSQLType(p.kind, id) && p.kind.GoType == id.GoType {
				return Formal[V]{&r.params[i]}
			}
		}
	}
	panic("plan.FormalOf: unknown parameter or mismatched type")
}

// RoutineArgument assigns a typed expression to a signature's formal.
type RoutineArgument struct {
	param *RoutineParameter
	part  expressionPart
	kind  catalogType
}

// Arg pairs a formal with an expression of the same Go type. Registration
// checks its PostgreSQL type and emits an explicit cast at the call site.
func Arg[V any](formal Formal[V], value Expr[V]) RoutineArgument {
	return RoutineArgument{formal.param, value.part, typeID(value.kind)}
}

func (r *Routine[V]) arguments(c *typedCompiler, args []RoutineArgument) ([]string, error) {
	if len(args) != len(r.params) {
		return nil, fmt.Errorf("tabula: %s needs %d routine arguments", r.requirement.Signature, len(r.params))
	}
	values := make(map[*RoutineParameter]expressionPart, len(args))
	for _, a := range args {
		if a.param == nil || a.part == nil || values[a.param] != nil || !sameSQLType(a.param.kind, a.kind) {
			return nil, fmt.Errorf("tabula: invalid, duplicate or mistyped routine argument")
		}
		values[a.param] = a.part
	}
	sql := make([]string, len(r.params))
	for i, p := range r.params {
		value := values[&r.params[i]]
		if value == nil {
			return nil, fmt.Errorf("tabula: missing argument for %s parameter %q", r.requirement.Signature, p.name)
		}
		arg, err := renderTypedArgument(c, value, p.kind)
		if err != nil {
			return nil, err
		}
		sql[i] = arg
	}
	return sql, nil
}

// Preserve the exact PostgreSQL type of each function argument at the call site.
func renderTypedArgument(c *typedCompiler, part expressionPart, kind catalogType) (string, error) {
	expr, err := part.render(c)
	if err != nil {
		return "", err
	}
	if _, slot := part.(*typedInput); slot {
		return expr, nil
	}
	return kind.Cast(expr), nil
}
