package tabula

import (
	"fmt"
	"reflect"
)

// OutputField is a named projection target on a registered View.
type OutputField[V any] struct {
	target *registration
	name   string
	index  int
	kind   PGType[V]
	null   bool
}

// Output resolves a view column and checks its Go type at definition time.
func Output[T, S, V any](view *View[T, S], name string, kind PGType[V]) OutputField[V] {
	if view == nil || view.entry == nil || kind.id.goType == nil {
		panic("tabula.Output: a registered view and a valid type are required")
	}
	i, ok := view.byName[name]
	if !ok || !typedIdent(name) {
		panic(fmt.Sprintf("tabula.Output: unknown output %q", name))
	}
	col := view.cols[i]
	if col.goType != kind.id.goType && (col.goType.Kind() != reflect.Pointer || col.goType.Elem() != kind.id.goType) {
		panic(fmt.Sprintf("tabula.Output: output %q holds %v, declared %v", name, col.goType, kind.id.goType))
	}
	return OutputField[V]{view.entry, name, i, kind, col.nullable}
}

// Projection assigns a typed expression to one output column.
type Projection struct {
	target     *registration
	index      int
	field      ShapeField
	part       expressionPart
	compatible bool
}

// Project pairs an output target with an expression of the same Go value type.
// Compilation also checks the PostgreSQL types, nullability and output order.
func Project[V any](target OutputField[V], value Expr[V]) Projection {
	field := ShapeField{Name: target.name, Type: target.kind.id.qualified(), Nullable: value.nullable}
	compatible := sameSQLType(target.kind.id, value.kind.id) && (!value.nullable || target.null)
	return Projection{target.target, target.index, field, value.part, compatible}
}

func (t catalogType) qualified() string {
	if t.element != nil {
		return t.element.qualified() + "[]"
	}
	return typedQuote(t.schema) + "." + typedQuote(t.name)
}

func (c *typedCompiler) project(owner *registration, projections []Projection) ([]string, []ShapeField, error) {
	return c.projectFields(owner, projections, nil)
}

func (c *typedCompiler) projectFields(owner *registration, projections []Projection, indexes []int) ([]string, []ShapeField, error) {
	if owner == nil || len(projections) == 0 {
		return nil, nil, fmt.Errorf("tabula: a registered output and at least one projected field are required")
	}
	width := len(owner.def.Columns)
	if indexes != nil {
		width = len(indexes)
	}
	if len(projections) != width {
		return nil, nil, fmt.Errorf("tabula: expected %d projected fields, got %d", width, len(projections))
	}
	selects := make([]string, len(projections))
	fields := make([]ShapeField, len(projections))
	for i, projection := range projections {
		index := i
		if indexes != nil {
			index = indexes[i]
		}
		if index < 0 || index >= len(owner.def.Columns) || projection.target != owner || projection.index != index || !projection.compatible || projection.part == nil {
			return nil, nil, fmt.Errorf("tabula: incompatible projection for output %q at position %d", projection.field.Name, i+1)
		}
		c.hasSecret = c.hasSecret || owner.def.Columns[index].Secret
		part, err := projection.part.render(c)
		if err != nil {
			return nil, nil, err
		}
		selects[i] = part + " AS " + typedQuote(projection.field.Name)
		fields[i] = projection.field
	}
	return selects, fields, nil
}
