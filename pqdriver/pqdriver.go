// Package pqdriver provides PostgreSQL SQLSTATE helpers and lib/pq-backed
// array column converters for tabula. Isolating lib/pq here keeps tabula
// itself driver-agnostic (so a pgxdriver can sit alongside without changing
// the core descriptor engine).
package pqdriver

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"

	"github.com/lib/pq"
)

// SQLState returns the five-character PostgreSQL SQLSTATE code carried by err
// (compatible with both lib/pq and pgx errors implementing SQLState() string),
// or "" when err carries none.
func SQLState(err error) string {
	if pg, ok := errors.AsType[interface {
		error
		SQLState() string
	}](err); ok {
		return pg.SQLState()
	}
	return ""
}

// IsUniqueViolation reports PostgreSQL unique_violation (SQLSTATE 23505).
func IsUniqueViolation(err error) bool { return SQLState(err) == "23505" }

// IsRejectedValue reports a value refused by column definitions: string data
// right truncation (22001), numeric value out of range (22003), not_null
// violation (23502), check violation (23514), or invalid text representation
// (22P02).
func IsRejectedValue(err error) bool {
	switch SQLState(err) {
	case "22001", "22003", "23502", "23514", "22P02":
		return true
	}
	return false
}

// IsNumericOutOfRange reports SQLSTATE 22003.
func IsNumericOutOfRange(err error) bool { return SQLState(err) == "22003" }

// TextArray addresses a PostgreSQL text[] / varchar[] column held as a slice
// of string or named string enum type (e.g. []core.Factor). NULL scans as nil.
func TextArray[E ~string](p *[]E) any { return &textArrayValue[E]{p} }

type textArrayValue[E ~string] struct{ p *[]E }

func (a *textArrayValue[E]) Scan(src any) error {
	if src == nil {
		*a.p = nil
		return nil
	}
	var raw pq.StringArray
	if err := raw.Scan(src); err != nil {
		return err
	}
	out := make([]E, len(raw))
	for i, s := range raw {
		out[i] = E(s)
	}
	*a.p = out
	return nil
}

func (a *textArrayValue[E]) Value() (driver.Value, error) {
	if *a.p == nil {
		return nil, nil
	}
	raw := make(pq.StringArray, len(*a.p))
	for i, e := range *a.p {
		raw[i] = string(e)
	}
	return raw.Value()
}

// Int64Array addresses a PostgreSQL bigint[] column held as a slice of int64
// or named int64 type. NULL scans as nil.
func Int64Array[E ~int64](p *[]E) any { return &int64ArrayValue[E]{p} }

type int64ArrayValue[E ~int64] struct{ p *[]E }

func (a *int64ArrayValue[E]) Scan(src any) error {
	if src == nil {
		*a.p = nil
		return nil
	}
	var raw pq.Int64Array
	if err := raw.Scan(src); err != nil {
		return err
	}
	out := make([]E, len(raw))
	for i, n := range raw {
		out[i] = E(n)
	}
	*a.p = out
	return nil
}

func (a *int64ArrayValue[E]) Value() (driver.Value, error) {
	if *a.p == nil {
		return nil, nil
	}
	raw := make(pq.Int64Array, len(*a.p))
	for i, e := range *a.p {
		raw[i] = int64(e)
	}
	return raw.Value()
}

// Array wraps a Go slice for PostgreSQL "= ANY($n)" binding via lib/pq.
func Array(v any) any { return pq.Array(v) }

// ArrayColumn resolves a db:"...,array" column's Go slice type ([]~string or
// []~int64) into a lib/pq-backed sql.Scanner / driver.Valuer converter.
func ArrayColumn(ft reflect.Type) (func(any) any, error) {
	if ft.Kind() != reflect.Slice {
		return nil, fmt.Errorf("array option requires a slice type, got %s", ft)
	}
	elem := ft.Elem()
	switch elem.Kind() {
	case reflect.String:
		return func(p any) any { return &reflectStringArray{ptr: p, sliceType: ft, elemType: elem} }, nil
	case reflect.Int64:
		return func(p any) any { return &reflectInt64Array{ptr: p, sliceType: ft, elemType: elem} }, nil
	default:
		return nil, fmt.Errorf("array option supports []~string or []~int64, got %s", ft)
	}
}

type reflectStringArray struct {
	ptr       any
	sliceType reflect.Type
	elemType  reflect.Type
}

func (a *reflectStringArray) Scan(src any) error {
	rv := reflect.ValueOf(a.ptr).Elem()
	if src == nil {
		rv.Set(reflect.Zero(a.sliceType))
		return nil
	}
	var raw pq.StringArray
	if err := raw.Scan(src); err != nil {
		return err
	}
	out := reflect.MakeSlice(a.sliceType, len(raw), len(raw))
	for i, s := range raw {
		out.Index(i).Set(reflect.ValueOf(s).Convert(a.elemType))
	}
	rv.Set(out)
	return nil
}

func (a *reflectStringArray) Value() (driver.Value, error) {
	rv := reflect.ValueOf(a.ptr).Elem()
	if rv.IsNil() {
		return nil, nil
	}
	raw := make(pq.StringArray, rv.Len())
	for i := range rv.Len() {
		raw[i] = rv.Index(i).String()
	}
	return raw.Value()
}

type reflectInt64Array struct {
	ptr       any
	sliceType reflect.Type
	elemType  reflect.Type
}

func (a *reflectInt64Array) Scan(src any) error {
	rv := reflect.ValueOf(a.ptr).Elem()
	if src == nil {
		rv.Set(reflect.Zero(a.sliceType))
		return nil
	}
	var raw pq.Int64Array
	if err := raw.Scan(src); err != nil {
		return err
	}
	out := reflect.MakeSlice(a.sliceType, len(raw), len(raw))
	for i, n := range raw {
		out.Index(i).Set(reflect.ValueOf(n).Convert(a.elemType))
	}
	rv.Set(out)
	return nil
}

func (a *reflectInt64Array) Value() (driver.Value, error) {
	rv := reflect.ValueOf(a.ptr).Elem()
	if rv.IsNil() {
		return nil, nil
	}
	raw := make(pq.Int64Array, rv.Len())
	for i := range rv.Len() {
		raw[i] = rv.Index(i).Int()
	}
	return raw.Value()
}
