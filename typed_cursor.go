package tabula

import (
	"fmt"
	"reflect"
	"strconv"
)

// AtExpr supplies one cursor component using the expression's declared codec.
// Its expression must match the corresponding projected order term.
func AtExpr[V any](expr Expr[V], value V) CursorValue {
	b := expr.kind.id.bind(nil, value)
	return CursorValue{expr: expr.scalarOperand(), value: b.value, err: b.err}
}

// NullAtExpr supplies a SQL NULL boundary for a nullable order expression.
func NullAtExpr[V any](expr Expr[V]) CursorValue { return CursorValue{expr: expr.scalarOperand()} }

func (v CursorValue) operand() scalarOperand {
	if f := v.field; f != nil {
		return scalarOperand{f, f.kind, f.nullable}
	}
	return v.expr
}

func (o *rowOrdering) termNullable(i int) bool {
	if o.nullable != nil {
		return o.nullable[i]
	}
	return o.terms[i].orderValue().nullable
}

// prepareOrderCursor stores descriptor indexes, not SELECT ordinals. Partial
// table projections can reorder columns without changing the decoded struct.
func prepareOrderCursor(o *rowOrdering, projection []Projection, outer map[*occurrence]bool) error {
	o.cursors = nil
	o.projectionIndexes = nil
	o.nullable = nil
	names := make(map[string]bool, len(o.terms))
	for i, term := range o.terms {
		v := term.orderValue()
		if !repeatableExpression(v.part) {
			return fmt.Errorf("tabula: keyset order requires repeatable expressions")
		}
		index := -1
		for _, projected := range projection {
			if sameExpression(v.part, projected.part) {
				index = projected.index
				break
			}
		}
		if index < 0 {
			return fmt.Errorf("tabula: keyset order expression %d must be projected", i+1)
		}
		name := "@after.expr." + strconv.Itoa(i+1)
		if f, ok := v.part.(*fieldRef); ok {
			name = "@after." + f.name
		}
		// A quoted field name may already contain a generated suffix.
		for names[name] {
			name += "." + strconv.Itoa(i+1)
		}
		names[name] = true
		o.cursors = append(o.cursors, &typedInput{name: name, kind: v.kind})
		o.projectionIndexes = append(o.projectionIndexes, index)
		o.nullable = append(o.nullable, v.nullableWith(outer))
	}
	return nil
}

func cursorForOrder(o *rowOrdering, values []CursorValue) (RowCursor, error) {
	if o == nil || len(values) != len(o.terms) || len(o.cursors) != len(o.terms) || len(o.terms) == 0 {
		return RowCursor{}, fmt.Errorf("tabula: unknown keyset order or incorrect cursor arity")
	}
	bindings := make([]Binding, len(values))
	for i, v := range values {
		if v.err != nil {
			return RowCursor{}, fmt.Errorf("tabula: encode cursor component %d: %w", i+1, v.err)
		}
		want, got := o.terms[i].orderValue(), v.operand()
		if !sameExpression(want.part, got.part) || !sameSQLType(want.kind, got.kind) || want.kind.goType != got.kind.goType || want.kind.encoder != got.kind.encoder || isNilBound(v.value) && !o.termNullable(i) {
			return RowCursor{}, fmt.Errorf("tabula: incompatible cursor component %d", i+1)
		}
		bindings[i] = Binding{input: o.cursors[i], value: v.value}
	}
	return RowCursor{o, bindings}, nil
}

func cursorFromRow[T, S any](owner *descriptor[T, S], o *rowOrdering, row T) (RowCursor, error) {
	if owner == nil || o == nil || len(o.projectionIndexes) != len(o.terms) {
		return RowCursor{}, fmt.Errorf("tabula: unknown keyset order")
	}
	values := make([]CursorValue, len(o.terms))
	for i, index := range o.projectionIndexes {
		kind := o.terms[i].orderValue().kind
		value := reflect.ValueOf(owner.cols[index].field(&row)).Elem()
		if value.Type() != kind.goType && value.Kind() == reflect.Pointer {
			if value.IsNil() {
				values[i] = CursorValue{expr: o.terms[i].orderValue()}
				continue
			}
			value = value.Elem()
		}
		if value.Type() != kind.goType {
			return RowCursor{}, fmt.Errorf("tabula: cursor result codec mismatch at component %d", i+1)
		}
		b := kind.bind(nil, value.Interface())
		values[i] = CursorValue{expr: o.terms[i].orderValue(), value: b.value, err: b.err}
	}
	return cursorForOrder(o, values)
}
