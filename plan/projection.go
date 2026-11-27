package plan

import (
	"fmt"
	"reflect"

	"github.com/0xJean/tabula"
)

// OutputField is a named projection target on a registered View.
type OutputField[V any] struct {
	target *registration
	name   string
	index  int
	kind   tabula.PGType[V]
	null   bool
	secret bool
	id     *queryFieldIdentity
}

// Output resolves a view column and checks its Go type at definition time.
func Output[T, S, V any](view *tabula.View[T, S], name string, kind tabula.PGType[V]) OutputField[V] {
	owner := planView(view)
	id := typeID(kind)
	if owner == nil || owner.Entry == nil || id.GoType == nil {
		panic("plan.Output: a registered view and a valid type are required")
	}
	i, ok := owner.ByName[name]
	if !ok || !typedIdent(name) {
		panic(fmt.Sprintf("plan.Output: unknown output %q", name))
	}
	col := owner.Columns[i]
	if col.GoType != id.GoType && (col.GoType.Kind() != reflect.Pointer || col.GoType.Elem() != id.GoType) {
		panic(fmt.Sprintf("plan.Output: output %q holds %v, declared %v", name, col.GoType, id.GoType))
	}
	return OutputField[V]{owner.Entry, name, i, kind, col.Nullable, col.Secret, new(queryFieldIdentity)}
}

// Projection assigns a typed expression to one output column.
type Projection struct {
	target     *registration
	index      int
	field      tabula.ShapeField
	part       expressionPart
	compatible bool
	kind       catalogType
	id         *queryFieldIdentity
	secret     bool
}

// Project pairs an output target with an expression of the same Go value type.
// Compilation also checks the PostgreSQL types, nullability and output order.
func Project[V any](target OutputField[V], value Expr[V]) Projection {
	targetID := typeID(target.kind)
	valID := typeID(value.kind)
	field := tabula.ShapeField{Name: target.name, Type: targetID.Qualified(), Nullable: value.nullable}
	compatible := sameSQLType(targetID, valID) && (!value.nullable || target.null)
	return Projection{target.target, target.index, field, value.part, compatible, valID, target.id, target.secret}
}

func (c *typedCompiler) project(owner *outputLayout, projections []Projection) ([]string, []tabula.ShapeField, error) {
	return c.projectFields(owner, projections, nil)
}

func (c *typedCompiler) projectFields(owner *outputLayout, projections []Projection, indexes []int) ([]string, []tabula.ShapeField, error) {
	if owner == nil || owner.entry == nil || len(projections) == 0 {
		return nil, nil, fmt.Errorf("tabula: a registered output and at least one projected field are required")
	}
	width := len(owner.secrets)
	if indexes != nil {
		width = len(indexes)
	}
	if len(projections) != width {
		return nil, nil, fmt.Errorf("tabula: expected %d projected fields, got %d", width, len(projections))
	}
	selects := make([]string, len(projections))
	fields := make([]tabula.ShapeField, len(projections))
	for i, projection := range projections {
		index := i
		if indexes != nil {
			index = indexes[i]
		}
		if index < 0 || index >= len(owner.secrets) || projection.target != owner.entry || projection.index != index || !projection.compatible || projection.part == nil {
			return nil, nil, fmt.Errorf("tabula: incompatible projection for output %q at position %d", projection.field.Name, i+1)
		}
		c.hasSecret = c.hasSecret || owner.secrets[index]
		part, err := projection.part.render(c)
		if err != nil {
			return nil, nil, err
		}
		selects[i] = part + " AS " + typedQuote(projection.field.Name)
		fields[i] = projection.field
	}
	return selects, fields, nil
}
