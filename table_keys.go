package tabula

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// SoftDelete configures status-value soft deletion on a ",deleted" column.
// Leave TableSpec.SoftDelete nil for *time.Time (deleted_at IS NULL) or bool
// (is_deleted = FALSE).
type SoftDelete struct {
	// DeletedValue is the sentinel value written to mark a row deleted
	// (must be string/~string or an integer matching the ",deleted" field type).
	DeletedValue any
	// AliveValue, when non-nil, sets the live-row predicate to "<col> = <AliveValue>".
	// When nil, the live-row predicate is "<col> <> <DeletedValue>".
	AliveValue any
}

type deleteKind int

const (
	deleteNone deleteKind = iota
	deleteTimestamp
	deleteBool
	deleteStatus
)

type deleteSpec struct {
	kind       deleteKind
	aliveSQL   string
	deletedVal reflect.Value
	excludeIns bool
}

func (t *Table[T, K, S]) bindKeys(fail func(string, ...any) error) error {
	if len(t.keys) == 0 {
		return fail("key column missing")
	}
	kt := reflect.TypeFor[K]()
	if !kt.Comparable() {
		return fail("key type %s is not comparable", kt)
	}
	// Single-column key where K matches the key column's exact Go type.
	if len(t.keys) == 1 {
		col := t.cols[t.keys[0]]
		if col.goType == kt {
			t.keyBinders = []func(*K) any{func(k *K) any { return col.wrap(k) }}
			t.keyBuilder = func(row *T) K { return *col.field(row).(*K) }
			return nil
		}
		if kt.Kind() != reflect.Struct {
			return fail("key column %s is %s, not %s", col.name, col.goType, kt)
		}
	}
	if t.serial {
		return fail("serial requires a single integer key column")
	}
	if kt.Kind() != reflect.Struct {
		return fail("composite key type %s must be a struct", kt)
	}

	// Match exported struct fields on K to key columns on T by db tag or field name.
	keyColByName := make(map[string]int, len(t.keys))
	keyColByField := make(map[string]int, len(t.keys))
	for pos, colIdx := range t.keys {
		keyColByName[t.cols[colIdx].name] = pos
		if t.cols[colIdx].fieldName != "" {
			keyColByField[t.cols[colIdx].fieldName] = pos
		}
	}

	keyFieldIdx := make([][]int, len(t.keys))
	matched := make([]bool, len(t.keys))
	for i := range kt.NumField() {
		f := kt.Field(i)
		if !f.IsExported() {
			continue
		}
		tag, hasTag := f.Tag.Lookup("db")
		if tag == "-" {
			continue
		}
		var (
			pos int
			ok  bool
		)
		if hasTag && tag != "" {
			colName := strings.Split(tag, ",")[0]
			pos, ok = keyColByName[colName]
		} else {
			pos, ok = keyColByField[f.Name]
		}
		if !ok {
			return fail("composite key field %s does not match any key column", f.Name)
		}
		if matched[pos] {
			return fail("composite key column %s matched twice", t.cols[t.keys[pos]].name)
		}
		col := t.cols[t.keys[pos]]
		if f.Type != col.goType {
			return fail("composite key field %s is %s, not %s", f.Name, f.Type, col.goType)
		}
		matched[pos] = true
		keyFieldIdx[pos] = f.Index
	}
	for pos, ok := range matched {
		if !ok {
			return fail("composite key type %s is missing key column %s", kt, t.cols[t.keys[pos]].name)
		}
	}

	t.keyBinders = make([]func(*K) any, len(t.keys))
	for pos, colIdx := range t.keys {
		idx := keyFieldIdx[pos]
		wrap := t.cols[colIdx].wrap
		t.keyBinders[pos] = func(k *K) any {
			ptr := reflect.ValueOf(k).Elem().FieldByIndex(idx).Addr().Interface()
			return wrap(ptr)
		}
	}
	t.keyBuilder = func(row *T) K {
		var k K
		kv := reflect.ValueOf(&k).Elem()
		for pos, colIdx := range t.keys {
			val := reflect.ValueOf(t.cols[colIdx].field(row)).Elem()
			kv.FieldByIndex(keyFieldIdx[pos]).Set(val)
		}
		return k
	}
	return nil
}

func (t *Table[T, K, S]) bindSoftDelete(spec *SoftDelete, fail func(string, ...any) error) error {
	if t.deleted < 0 {
		if spec != nil {
			return fail("SoftDelete config requires a deleted column")
		}
		return nil
	}
	col := t.cols[t.deleted]
	ft := col.goType
	switch {
	case ft == reflect.TypeFor[*time.Time]():
		if spec != nil {
			return fail("SoftDelete config is not used with *time.Time deleted column %s", col.name)
		}
		t.del = deleteSpec{
			kind:       deleteTimestamp,
			aliveSQL:   col.name + " IS NULL",
			excludeIns: true,
		}
		return nil
	case ft.Kind() == reflect.Bool:
		if spec != nil {
			return fail("SoftDelete config is not used with bool deleted column %s", col.name)
		}
		t.del = deleteSpec{
			kind:     deleteBool,
			aliveSQL: col.name + " = FALSE",
		}
		return nil
	case ft.Kind() == reflect.String || integer(ft):
		if spec == nil || spec.DeletedValue == nil {
			return fail("deleted column %s (%s) requires SoftDelete.DeletedValue", col.name, ft)
		}
		delVal, delLit, err := coerceDeleteLiteral(spec.DeletedValue, ft)
		if err != nil {
			return fail("SoftDelete.DeletedValue for %s: %w", col.name, err)
		}
		aliveSQL := col.name + " <> " + delLit
		if spec.AliveValue != nil {
			aliveVal, aliveLit, err := coerceDeleteLiteral(spec.AliveValue, ft)
			if err != nil {
				return fail("SoftDelete.AliveValue for %s: %w", col.name, err)
			}
			if reflect.DeepEqual(aliveVal.Interface(), delVal.Interface()) {
				return fail("SoftDelete.AliveValue and DeletedValue must differ on %s", col.name)
			}
			aliveSQL = col.name + " = " + aliveLit
		}
		t.del = deleteSpec{
			kind:       deleteStatus,
			aliveSQL:   aliveSQL,
			deletedVal: delVal,
		}
		return nil
	default:
		return fail("deleted column %s must be *time.Time, bool, string, or integer (got %s)", col.name, ft)
	}
}

func coerceDeleteLiteral(v any, target reflect.Type) (reflect.Value, string, error) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return reflect.Value{}, "", fmt.Errorf("value is nil")
	}
	if !rv.Type().ConvertibleTo(target) {
		return reflect.Value{}, "", fmt.Errorf("type %s is not convertible to %s", rv.Type(), target)
	}
	// Prevent converting an integer into a string (Go allows string(int)).
	if target.Kind() == reflect.String && rv.Kind() != reflect.String {
		return reflect.Value{}, "", fmt.Errorf("expected string for %s, got %s", target, rv.Type())
	}
	if integer(target) && !integer(rv.Type()) {
		return reflect.Value{}, "", fmt.Errorf("expected integer for %s, got %s", target, rv.Type())
	}
	converted := rv.Convert(target)
	if target.Kind() == reflect.String {
		s := converted.String()
		if strings.ContainsRune(s, 0) {
			return reflect.Value{}, "", fmt.Errorf("string literal contains NUL byte")
		}
		return converted, "'" + strings.ReplaceAll(s, "'", "''") + "'", nil
	}
	switch target.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return converted, strconv.FormatInt(converted.Int(), 10), nil
	default:
		return converted, strconv.FormatUint(converted.Uint(), 10), nil
	}
}

func (t *Table[T, K, S]) isRowDeleted(row *T) bool {
	if t.deleted < 0 {
		return false
	}
	col := t.cols[t.deleted]
	switch t.del.kind {
	case deleteTimestamp:
		at := col.field(row).(**time.Time)
		return *at != nil
	case deleteBool:
		return reflect.ValueOf(col.field(row)).Elem().Bool()
	case deleteStatus:
		got := reflect.ValueOf(col.field(row)).Elem().Interface()
		return reflect.DeepEqual(got, t.del.deletedVal.Interface())
	default:
		return false
	}
}

func (t *Table[T, K, S]) resolveReturningColumns(columns []string) []int {
	if len(columns) == 0 {
		out := make([]int, len(t.cols))
		for i := range t.cols {
			out[i] = i
		}
		return out
	}
	out := make([]int, len(columns))
	for n, name := range columns {
		idx, ok := t.byName[name]
		if !ok {
			panic(t.prefix() + ".Table " + t.name + ": column not declared: " + name)
		}
		out[n] = idx
	}
	return out
}

func (t *Table[T, K, S]) columnList(idx []int) string {
	names := make([]string, len(idx))
	for n, i := range idx {
		names[n] = t.cols[i].name
	}
	return strings.Join(names, ", ")
}

func (t *Table[T, K, S]) insertSecretMask() []bool {
	out := make([]bool, len(t.stmt.insertColumns))
	for n, i := range t.stmt.insertColumns {
		out[n] = t.cols[i].secret
	}
	return out
}
