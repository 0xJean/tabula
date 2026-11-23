package tabula

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"reflect"
	"time"
	"unsafe"
)

// fieldAccessor builds a fast pointer-to-field function at Define time using
// the struct's static byte offset, avoiding reflect.ValueOf + FieldByIndex on
// every scanned row.
func fieldAccessor[T any](rt reflect.Type, index []int) func(*T) any {
	var offset uintptr
	cur := rt
	for _, i := range index {
		f := cur.Field(i)
		offset += f.Offset
		cur = f.Type
	}
	switch cur {
	case reflect.TypeFor[int64]():
		return func(row *T) any { return (*int64)(unsafe.Add(unsafe.Pointer(row), offset)) }
	case reflect.TypeFor[int]():
		return func(row *T) any { return (*int)(unsafe.Add(unsafe.Pointer(row), offset)) }
	case reflect.TypeFor[int32]():
		return func(row *T) any { return (*int32)(unsafe.Add(unsafe.Pointer(row), offset)) }
	case reflect.TypeFor[string]():
		return func(row *T) any { return (*string)(unsafe.Add(unsafe.Pointer(row), offset)) }
	case reflect.TypeFor[bool]():
		return func(row *T) any { return (*bool)(unsafe.Add(unsafe.Pointer(row), offset)) }
	case reflect.TypeFor[float64]():
		return func(row *T) any { return (*float64)(unsafe.Add(unsafe.Pointer(row), offset)) }
	case reflect.TypeFor[time.Time]():
		return func(row *T) any { return (*time.Time)(unsafe.Add(unsafe.Pointer(row), offset)) }
	case reflect.TypeFor[*time.Time]():
		return func(row *T) any { return (**time.Time)(unsafe.Add(unsafe.Pointer(row), offset)) }
	case reflect.TypeFor[*string]():
		return func(row *T) any { return (**string)(unsafe.Add(unsafe.Pointer(row), offset)) }
	case reflect.TypeFor[*int64]():
		return func(row *T) any { return (**int64)(unsafe.Add(unsafe.Pointer(row), offset)) }
	case reflect.TypeFor[[]byte]():
		return func(row *T) any { return (*[]byte)(unsafe.Add(unsafe.Pointer(row), offset)) }
	case reflect.TypeFor[[]string]():
		return func(row *T) any { return (*[]string)(unsafe.Add(unsafe.Pointer(row), offset)) }
	default:
		ft := cur
		return func(row *T) any { return reflect.NewAt(ft, unsafe.Add(unsafe.Pointer(row), offset)).Interface() }
	}
}

var (
	scannerType = reflect.TypeFor[sql.Scanner]()
	valuerType  = reflect.TypeFor[driver.Valuer]()
	timeType    = reflect.TypeFor[time.Time]()
)

func zeroOf[V any](p any) any { return &ZeroValue[V]{p.(*V)} }
func raw(p any) any           { return p }

// Resolve returns the converter of a pointer to a value of type ft in r:
//  1. json option: json text in and out;
//  2. zero option: registered scalar, NULL read as the zero value;
//  3. registry types (registered via RegisterType / TenantWrap);
//  4. types whose pointer implements sql.Scanner and driver.Valuer;
//  5. database/sql native primitives (bool, ints, floats, string, []byte, time.Time).
func (r *Registry[S]) Resolve(ft reflect.Type, jsonOption, zeroOption bool) (func(any) any, error) {
	return r.resolve(ft, jsonOption, zeroOption, false)
}

func (r *Registry[S]) resolve(ft reflect.Type, jsonOption, zeroOption, arrayOption bool) (func(any) any, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	switch {
	case arrayOption:
		if r.arrayColFn == nil {
			return nil, fmt.Errorf("array option requires RegistryConfig.ArrayColumn for type %s", ft)
		}
		return r.arrayColFn(ft)
	case jsonOption:
		return JSON, nil
	case zeroOption:
		if wrap, ok := r.zeros[ft]; ok {
			return wrap, nil
		}
		return nil, fmt.Errorf("zero option has no registered type %s", ft)
	}
	if wrap, ok := r.types[ft]; ok {
		return wrap, nil
	}
	base := ft
	if base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	if scanner(reflect.PointerTo(base)) && reflect.PointerTo(base).Implements(valuerType) {
		return raw, nil
	}
	switch base.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.String:
		return raw, nil
	case reflect.Slice:
		if base.Elem().Kind() == reflect.Uint8 {
			return raw, nil
		}
	case reflect.Struct:
		if base == timeType {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("unsupported Go type %s (implement sql.Scanner and driver.Valuer, or use json/zero)", ft)
}

func scanner(t reflect.Type) bool { return t != nil && t.Implements(scannerType) }

// JSONValue holds a pointer to any Go value of a json/jsonb column.
type JSONValue struct{ p any }

// JSON addresses a json/jsonb column held as any Go value (text in and out).
// NULL reads as the zero value; a nil pointer value writes NULL.
func JSON(p any) any { return &JSONValue{p} }

func (v *JSONValue) Scan(src any) error {
	reflect.ValueOf(v.p).Elem().SetZero()
	switch s := src.(type) {
	case nil:
		return nil
	case []byte:
		return json.Unmarshal(s, v.p)
	case string:
		return json.Unmarshal([]byte(s), v.p)
	}
	return fmt.Errorf("json column: unsupported %T", src)
}

func (v *JSONValue) Value() (driver.Value, error) {
	if e := reflect.ValueOf(v.p).Elem(); e.Kind() == reflect.Pointer && e.IsNil() {
		return nil, nil
	}
	b, err := json.Marshal(v.p)
	return string(b), err
}

// ZeroValue holds a nullable column as a plain Go value: NULL reads as the
// zero value and the value is bound as itself.
type ZeroValue[V any] struct{ p *V }

// Zero addresses a nullable scalar column as a plain value of type V.
func Zero[V any](p *V) any { return &ZeroValue[V]{p} }

func (v *ZeroValue[V]) Scan(src any) error {
	var n sql.Null[V]
	if err := n.Scan(src); err != nil {
		return err
	}
	*v.p = n.V
	return nil
}

func (v *ZeroValue[V]) Value() (driver.Value, error) {
	return driver.DefaultParameterConverter.ConvertValue(*v.p)
}
