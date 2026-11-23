package tabula

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// View describes a read-only row projection T (such as a JOIN result, summary,
// or auxiliary lookup table) without requiring a key, tenant, deleted, or
// version column. It reuses the Registry's db-tag codecs (UUID, JSON, Zero,
// Array, custom Scanner/Valuer) so callers never write sql.Null* scan structs.
type View[T, S any] struct {
	Name    string
	Columns []Column[T]

	reg     *Registry[S]
	cols    []column[T]
	byName  map[string]int
	colList string
	viewID  int // 1-based index in reg.views; 0 when undefined
}

// ViewDefinition is one defined read-only projection's metadata.
type ViewDefinition struct {
	Name       string
	Columns    []DefinedColumn
	Statements map[string]string
}

func cloneViewDefinition(v ViewDefinition) ViewDefinition {
	return ViewDefinition{
		Name:       v.Name,
		Columns:    slices.Clone(v.Columns),
		Statements: maps.Clone(v.Statements),
	}
}

// DefineView validates v against r (panicking on declaration mistakes at
// package initialization) and records it in r.DefinedViews().
func DefineView[T, S any](r *Registry[S], v View[T, S]) *View[T, S] {
	d, err := TryDefineView(r, v)
	if err != nil {
		panic(err)
	}
	cols := make([]DefinedColumn, len(d.cols))
	for i, c := range d.cols {
		cols[i] = DefinedColumn{Name: c.name, GoType: c.goType.String(), Nullable: c.nullable}
	}
	d.viewID = r.recordView(d.Name, cols)
	return d
}

// TryDefineView validates v without recording it in r.
func TryDefineView[T, S any](r *Registry[S], v View[T, S]) (*View[T, S], error) {
	if r == nil {
		return nil, errors.New("tabula.DefineView: registry is required")
	}
	if v.Name == "" {
		return nil, errors.New(r.prefix + ".DefineView: name is required")
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf(r.prefix+".DefineView "+v.Name+": "+format, args...)
	}
	rt := reflect.TypeFor[T]()
	if rt.Kind() != reflect.Struct {
		return nil, fail("row type %s is not a struct", rt)
	}
	v.reg = r
	v.cols, v.byName, v.viewID = nil, map[string]int{}, 0
	if err := v.fields(rt, nil, true, fail); err != nil {
		return nil, err
	}
	for _, c := range v.Columns {
		if err := v.escape(c, fail); err != nil {
			return nil, err
		}
	}
	if len(v.cols) == 0 {
		return nil, fail("at least one column is required")
	}
	names := make([]string, len(v.cols))
	for i, c := range v.cols {
		names[i] = c.name
	}
	v.colList = strings.Join(names, ", ")
	return &v, nil
}

func (v *View[T, S]) fields(rt reflect.Type, prefix []int, top bool, fail func(string, ...any) error) error {
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		index := append(slices.Clone(prefix), i)
		tag, tagged := f.Tag.Lookup("db")
		switch {
		case tag == "-":
			continue
		case !tagged && top && f.Anonymous && f.Type.Kind() == reflect.Struct:
			if err := v.fields(f.Type, index, false, fail); err != nil {
				return err
			}
			continue
		case !tagged:
			return fail(`exported field %s has no db tag (use db:"-" to skip)`, f.Name)
		}
		if err := v.tagged(f, index, tag, fail); err != nil {
			return err
		}
	}
	return nil
}

func (v *View[T, S]) tagged(f reflect.StructField, index []int, tag string, fail func(string, ...any) error) error {
	parts := strings.Split(tag, ",")
	name := parts[0]
	if name == "" {
		return fail("field %s: db tag has no column name", f.Name)
	}
	options := map[string]bool{}
	for _, option := range parts[1:] {
		switch option {
		case "json", "zero", "array":
			options[option] = true
		default:
			return fail("column %s: option %s is not supported on a View", name, option)
		}
	}
	switch {
	case options["json"] && options["zero"]:
		return fail("column %s: option json cannot combine with zero", name)
	case options["array"] && options["json"]:
		return fail("column %s: option array cannot combine with json", name)
	case options["array"] && options["zero"]:
		return fail("column %s: option array cannot combine with zero", name)
	}
	wrap, err := v.reg.resolve(f.Type, options["json"], options["zero"], options["array"])
	if err != nil {
		return fail("column %s: %w", name, err)
	}
	field := func(row *T) any { return reflect.ValueOf(row).Elem().FieldByIndex(index).Addr().Interface() }
	return v.add(column[T]{
		name:     name,
		field:    field,
		goType:   f.Type,
		nullable: f.Type.Kind() == reflect.Pointer || options["zero"] || options["array"],
		target:   func(row *T) any { return wrap(field(row)) },
		wrap:     wrap,
	}, fail)
}

func (v *View[T, S]) escape(c Column[T], fail func(string, ...any) error) error {
	if c.Name == "" || c.Addr == nil {
		return fail("Col %q needs a name and an address", c.Name)
	}
	target := c.Addr
	vt := reflect.TypeOf(c.Addr(new(T)))
	if !scanner(vt) {
		if vt == nil || vt.Kind() != reflect.Pointer {
			return fail("column %s: Col must address a field, got %v", c.Name, vt)
		}
		wrap, err := v.reg.Resolve(vt.Elem(), false, false)
		if err != nil {
			return fail("column %s: %w", c.Name, err)
		}
		target = func(row *T) any { return wrap(c.Addr(row)) }
	}
	held := vt
	if held.Kind() == reflect.Pointer {
		held = held.Elem()
	}
	nullable := held.Kind() == reflect.Pointer || strings.HasPrefix(held.Name(), "Null")
	return v.add(column[T]{name: c.Name, target: target, goType: held, nullable: nullable}, fail)
}

func (v *View[T, S]) add(c column[T], fail func(string, ...any) error) error {
	if _, ok := v.byName[c.name]; ok {
		return fail("column %s declared twice", c.name)
	}
	v.byName[c.name] = len(v.cols)
	v.cols = append(v.cols, c)
	return nil
}

func (v *View[T, S]) prefix() string {
	if v.reg != nil {
		return v.reg.prefix
	}
	return "tabula"
}

// Cols returns the comma-separated select column list in struct field order.
func (v *View[T, S]) Cols() string { return v.colList }

// ColsAs returns the select column list qualified with table alias (e.g. "c.id, c.name").
func (v *View[T, S]) ColsAs(alias string) string {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return v.colList
	}
	out := make([]string, len(v.cols))
	for i, c := range v.cols {
		out[i] = alias + "." + c.name
	}
	return strings.Join(out, ", ")
}

// Statement records a named read statement on this View's ViewDefinition.
func (v *View[T, S]) Statement(key, sql string) string {
	if v.reg == nil || v.viewID <= 0 {
		panic(v.prefix() + ".ViewStatement " + v.Name + ": view is not defined")
	}
	v.reg.mu.Lock()
	defer v.reg.mu.Unlock()
	if v.viewID > len(v.reg.views) {
		panic(v.prefix() + ".ViewStatement " + v.Name + ": view is not defined")
	}
	d := &v.reg.views[v.viewID-1]
	if _, ok := d.Statements[key]; ok {
		panic(fmt.Sprintf("%s.ViewStatement %s: key %s declared twice", v.prefix(), v.Name, key))
	}
	d.Statements[key] = sql
	return sql
}

// Definition returns a deep copy of this View's metadata.
func (v *View[T, S]) Definition() ViewDefinition {
	if v.reg == nil || v.viewID <= 0 {
		panic(v.prefix() + ".ViewDefinition " + v.Name + ": view is not defined")
	}
	v.reg.mu.RLock()
	defer v.reg.mu.RUnlock()
	if v.viewID > len(v.reg.views) {
		panic(v.prefix() + ".ViewDefinition " + v.Name + ": view is not defined")
	}
	return cloneViewDefinition(v.reg.views[v.viewID-1])
}

// Bind binds this View to a transaction Session for read execution.
func (v *View[T, S]) Bind(s Session[S]) *ViewRows[T, S] { return &ViewRows[T, S]{v: v, s: s} }

// ViewRows executes read-only queries and decodes rows into T using View's db tags.
type ViewRows[T, S any] struct {
	v *View[T, S]
	s Session[S]
}

func (r *ViewRows[T, S]) addrs(row *T) []any {
	out := make([]any, len(r.v.cols))
	for i, c := range r.v.cols {
		out[i] = c.target(row)
	}
	return out
}

// One executes query expecting at most one row; returns nil, nil on sql.ErrNoRows.
func (r *ViewRows[T, S]) One(ctx context.Context, scope S, query string, args ...any) (*T, error) {
	var row T
	err := r.s.Scan(ctx, scope, query, args, r.addrs(&row)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, r.v.reg.classifyErr(ctx, err)
	}
	return &row, nil
}

// Select executes query and decodes every row into []T.
func (r *ViewRows[T, S]) Select(ctx context.Context, scope S, query string, args ...any) ([]T, error) {
	var out []T
	err := r.s.Each(ctx, scope, query, args, func(scan func(...any) error) error {
		var row T
		if err := scan(r.addrs(&row)...); err != nil {
			return err
		}
		out = append(out, row)
		return nil
	})
	if err != nil {
		return nil, r.v.reg.classifyErr(ctx, err)
	}
	return out, nil
}

// Scan executes a scalar single-row query (e.g. COUNT/EXISTS) with the same error policy.
func (r *ViewRows[T, S]) Scan(ctx context.Context, scope S, query string, args []any, dest ...any) error {
	if err := r.s.Scan(ctx, scope, query, args, dest...); err != nil {
		return r.v.reg.classifyErr(ctx, err)
	}
	return nil
}
