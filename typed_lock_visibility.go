package tabula

import (
	"fmt"
	"reflect"
	"time"
)

// SoftDeleteColumn supplies the catalog type and encoder of a soft-delete
// field to LockedRowsSpec. Use a Field returned by FieldOf for the same
// relation occurrence as the row lock.
type SoftDeleteColumn interface {
	lockField() (*fieldRef, func(any) any)
}

func (f Field[V]) lockField() (*fieldRef, func(any) any) {
	if f.kind.encode == nil {
		return f.ref, nil
	}
	return f.ref, func(value any) any { return f.kind.encode(value.(V)) }
}

func lockVisibility[T, K, S any](c *typedCompiler, src *occurrence, table *Table[T, K, S], explicit SoftDeleteColumn) (string, Binding, error) {
	if table.deleted < 0 {
		if explicit != nil {
			return "", Binding{}, fmt.Errorf("tabula: table has no soft-delete column")
		}
		return "", Binding{}, nil
	}
	col := table.cols[table.deleted]
	if !typedIdent(col.name) {
		return "", Binding{}, fmt.Errorf("tabula: invalid soft-delete column %q", col.name)
	}
	var f *fieldRef
	var encode func(any) any
	if explicit != nil {
		f, encode = explicit.lockField()
		if f == nil || f.source != src || f.name != col.name {
			return "", Binding{}, fmt.Errorf("tabula: DeletedField must be the soft-delete field of the lock's occurrence")
		}
	} else {
		var kind catalogType
		switch col.goType {
		case reflect.TypeFor[bool]():
			kind = Bool().id
		case reflect.TypeFor[*time.Time]():
			kind = Timestamptz().id
		case reflect.TypeFor[int32]():
			kind = Int4().id
		case reflect.TypeFor[int64]():
			kind = Int8().id
		case reflect.TypeFor[string]():
			kind = Text().id
		default:
			return "", Binding{}, fmt.Errorf("tabula: declare the PostgreSQL type of %q with DeletedField", col.name)
		}
		f = &fieldRef{source: src, name: col.name, kind: kind, nullable: col.nullable, secret: col.secret}
	}
	column, err := f.render(c)
	if err != nil {
		return "", Binding{}, err
	}
	switch table.del.kind {
	case deleteTimestamp:
		return "(" + column + " IS NULL)", Binding{}, nil
	case deleteBool:
		if !sameSQLType(f.kind, Bool().id) {
			return "", Binding{}, fmt.Errorf("tabula: boolean soft deletion requires a pg_catalog.bool column")
		}
		return "(" + column + " = FALSE)", Binding{}, nil
	}
	operator, value := "<>", table.del.deletedVal
	if table.del.aliveVal.IsValid() {
		operator, value = "=", table.del.aliveVal
	}
	live := Binding{&typedInput{"@live", f.kind}, value.Interface()}
	if encode != nil {
		live.value = encode(live.value)
	}
	arg, err := live.input.render(c)
	if err != nil {
		return "", Binding{}, err
	}
	return "(" + column + " " + operator + " " + arg + ")", live, nil
}
