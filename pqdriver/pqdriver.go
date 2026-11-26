// Package pqdriver provides lib/pq-backed array converters for Tabula.
package pqdriver

import (
	"database/sql/driver"
	"fmt"
	"reflect"

	"github.com/lib/pq"
)

// TextArray addresses a text[] column, including named string elements.
func TextArray[E ~string](p *[]E) any { return arrayOf[string](reflect.TypeFor[[]E]())(p) }

// Int64Array addresses a bigint[] column, including named int64 elements.
func Int64Array[E ~int64](p *[]E) any { return arrayOf[int64](reflect.TypeFor[[]E]())(p) }

// Array wraps a Go slice for PostgreSQL "= ANY($n)" binding via lib/pq.
func Array(v any) any { return pq.Array(v) }

// ArrayColumn resolves one-dimensional bool, string, int16, int32, int64,
// uint32, float32 and float64 slices, including slices of scalar pointers.
// Named types and NULL elements retain their identities when scanned.
func ArrayColumn(ft reflect.Type) (func(any) any, error) {
	if ft.Kind() != reflect.Slice {
		return nil, fmt.Errorf("array option requires a slice type, got %s", ft)
	}
	switch ft.Elem().Kind() {
	case reflect.Pointer:
		return nullableArrayColumn(ft)
	case reflect.String:
		return arrayOf[string](ft), nil
	case reflect.Int16:
		return scannedArrayOf[int16], nil
	case reflect.Int32:
		return arrayOf[int32](ft), nil
	case reflect.Int64:
		return arrayOf[int64](ft), nil
	case reflect.Uint32:
		return scannedArrayOf[uint32], nil
	case reflect.Float32:
		return arrayOf[float32](ft), nil
	case reflect.Float64:
		return arrayOf[float64](ft), nil
	case reflect.Bool:
		return arrayOf[bool](ft), nil
	default:
		return nil, fmt.Errorf("unsupported array element type %s", ft.Elem())
	}
}

// lib/pq provides optimized scanners for the built-in slice types.
func arrayOf[E any](ft reflect.Type) func(any) any {
	if ft == reflect.TypeFor[[]E]() {
		return func(p any) any { return pq.Array(p) }
	}
	return func(p any) any { return namedArray[E]{reflect.ValueOf(p).Elem()} }
}

type namedArray[E any] struct{ dst reflect.Value }

func (a namedArray[E]) Scan(src any) error {
	var values []E
	if err := pq.Array(&values).Scan(src); err != nil {
		return err
	}
	if values == nil {
		a.dst.SetZero()
		return nil
	}
	out := reflect.MakeSlice(a.dst.Type(), len(values), len(values))
	for i, v := range values {
		out.Index(i).Set(reflect.ValueOf(v).Convert(out.Type().Elem()))
	}
	a.dst.Set(out)
	return nil
}

func (a namedArray[E]) Value() (driver.Value, error) {
	if a.dst.IsNil() {
		return nil, nil
	}
	out := make([]E, a.dst.Len())
	for i := range out {
		out[i] = a.dst.Index(i).Convert(reflect.TypeFor[E]()).Interface().(E)
	}
	return pq.Array(out).Value()
}
