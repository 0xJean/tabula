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
		return scannedArrayOf[bool], nil
	case reflect.String:
		return scannedArrayOf[string], nil
	case reflect.Int16:
		return scannedArrayOf[int16], nil
	case reflect.Int32:
		return scannedArrayOf[int32], nil
	case reflect.Int64:
		return scannedArrayOf[int64], nil
	case reflect.Uint32:
		return scannedArrayOf[uint32], nil
	case reflect.Float32:
		return scannedArrayOf[float32], nil
	case reflect.Float64:
		return scannedArrayOf[float64], nil
	default:
		return nil, fmt.Errorf("unsupported nullable array element type %s", ft.Elem())
	}
}

// lib/pq delegates element conversion to sql.Scanner for slices without
// a specialized codec. Decode into temporary storage to keep the destination
// intact on a conversion error or an unexpected NULL.
type scannedArray[E any] struct{ dst reflect.Value }

func scannedArrayOf[E any](ptr any) any {
	return scannedArray[E]{reflect.ValueOf(ptr).Elem()}
}

func (a scannedArray[E]) Scan(src any) error {
	var elements []sql.Null[E]
	if err := pq.Array(&elements).Scan(src); err != nil {
		return err
	}
	if elements == nil {
		a.dst.SetZero()
		return nil
	}
	out := reflect.MakeSlice(a.dst.Type(), len(elements), len(elements))
	kind := out.Type().Elem()
	for i, elem := range elements {
		dst := out.Index(i)
		if !elem.Valid {
			if kind.Kind() != reflect.Pointer {
				return fmt.Errorf("cannot scan NULL array element %d into %s", i, kind)
			}
			continue
		}
		if kind.Kind() == reflect.Pointer {
			dst.Set(reflect.New(kind.Elem()))
			dst = dst.Elem()
		}
		dst.Set(reflect.ValueOf(elem.V).Convert(dst.Type()))
	}
	a.dst.Set(out)
	return nil
}

func (a scannedArray[E]) Value() (driver.Value, error) {
	return pq.Array(a.dst.Interface()).Value()
}
