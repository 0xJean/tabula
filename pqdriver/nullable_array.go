package pqdriver

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"

	"github.com/lib/pq"
)

func nullableArrayColumn(ft reflect.Type) (func(any) any, error) {
	switch ft.Elem().Elem().Kind() {
	case reflect.Bool:
		return nullableArrayOf[bool], nil
	case reflect.String:
		return nullableArrayOf[string], nil
	case reflect.Int16:
		return nullableArrayOf[int16], nil
	case reflect.Int32:
		return nullableArrayOf[int32], nil
	case reflect.Int64:
		return nullableArrayOf[int64], nil
	case reflect.Uint32:
		return nullableArrayOf[uint32], nil
	case reflect.Float32:
		return nullableArrayOf[float32], nil
	case reflect.Float64:
		return nullableArrayOf[float64], nil
	default:
		return nil, fmt.Errorf("unsupported nullable array element type %s", ft.Elem())
	}
}

// lib/pq can scan individual array elements through sql.Scanner. sql.Null
// preserves their presence while database/sql decodes the primitive value.
type nullableArray[E any] struct{ dst reflect.Value }

func nullableArrayOf[E any](ptr any) any {
	return nullableArray[E]{reflect.ValueOf(ptr).Elem()}
}

func (a nullableArray[E]) Scan(src any) error {
	var elements []sql.Null[E]
	if err := pq.Array(&elements).Scan(src); err != nil {
		return err
	}
	if elements == nil {
		a.dst.SetZero()
		return nil
	}
	out := reflect.MakeSlice(a.dst.Type(), len(elements), len(elements))
	kind := out.Type().Elem().Elem()
	for i, elem := range elements {
		if elem.Valid {
			ptr := reflect.New(kind)
			ptr.Elem().Set(reflect.ValueOf(elem.V).Convert(kind))
			out.Index(i).Set(ptr)
		}
	}
	a.dst.Set(out)
	return nil
}

func (a nullableArray[E]) Value() (driver.Value, error) {
	return pq.Array(a.dst.Interface()).Value()
}
