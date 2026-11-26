package tabula

import (
	"bytes"
	"reflect"
)

// A pointer scope can use the mapping of its element, just like a nullable
// descriptor field. An explicitly mapped pointer retains its own encoder.
func (t catalogType) bindScope(scope any) Binding {
	v := reflect.ValueOf(scope)
	if v.Kind() == reflect.Pointer && v.Type().Elem() == t.goType {
		if v.IsNil() {
			return Binding{}
		}
		scope = v.Elem().Interface()
	}
	return t.bind(nil, scope)
}

func (t catalogType) bind(input *typedInput, value any) Binding {
	if t.encoder != nil {
		encoded, err := t.encoder.encode(value)
		if err != nil {
			return Binding{input: input, err: err}
		}
		value = encoded
	}
	return Binding{input: input, value: t.snapshot(value)}
}

// Keep the caller, canonical binding and per-statement driver storage
// independent. Array adapters may retain or edit the slice they receive.
func (t catalogType) snapshot(value any) any {
	if t.element == nil && (t.schema != "pg_catalog" || t.name != "bytea") {
		return value
	}
	v := reflect.ValueOf(value)
	if v.Kind() != reflect.Slice {
		return value
	}
	e := v.Type().Elem()
	if t.element == nil && e.Kind() != reflect.Uint8 {
		return value
	}
	if v.IsNil() {
		if t.element == nil {
			return nil
		}
		return value // array adapters accept a typed nil slice
	}
	clone := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
	reflect.Copy(clone, v)
	if e.Kind() == reflect.Pointer {
		for i := range clone.Len() {
			item := clone.Index(i)
			if item.IsNil() {
				continue
			}
			ptr := reflect.New(e.Elem())
			ptr.Elem().Set(item.Elem())
			if elem := ptr.Elem(); elem.Kind() == reflect.Slice && elem.Type().Elem().Kind() == reflect.Uint8 {
				elem.SetBytes(bytes.Clone(elem.Bytes()))
			}
			item.Set(ptr)
		}
	}
	// A byte string can itself be a mutable element of a one-dimensional
	// SQL array. Keep named slices such as raw documents in their Go type.
	if e.Kind() == reflect.Slice && e.Elem().Kind() == reflect.Uint8 {
		for i := range clone.Len() {
			raw := clone.Index(i)
			raw.SetBytes(bytes.Clone(raw.Bytes()))
		}
	}
	return clone.Interface()
}
