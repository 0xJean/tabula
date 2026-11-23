package tabula

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"reflect"
	"strings"
	"time"
)

// ViewSpec declares a read-only row projection T (such as a JOIN result, summary,
// or auxiliary lookup table) without requiring a key, scope, deleted, or
// version column. It reuses the Registry's db-tag codecs (UUID, JSON, Zero,
// Array, custom Scanner/Valuer) and supports nested struct column prefixes
// (e.g. `Order OrderRow` tagged `db:"o_"` maps o_id, o_code, ...).
type ViewSpec[T any] struct {
	Name    string
	Columns []Column[T]
}

// View is the compiled row shape for a hand-written query.
type View[T, S any] struct {
	descriptor[T, S]
	colList string
}

// ViewDefinition describes a registered query projection.
type ViewDefinition Definition

// Definition returns a deep copy of the registered view metadata.
func (v *View[T, S]) Definition() ViewDefinition { return ViewDefinition(v.descriptor.Definition()) }

// DefineView validates v against r (panicking on declaration mistakes at
// package initialization) and records it in r.DefinedViews().
func DefineView[T, S any](r *Registry[S], spec ViewSpec[T]) *View[T, S] {
	d, err := TryDefineView(r, spec)
	if err != nil {
		panic(err)
	}
	d.entry = r.record(Definition{Name: d.name, Columns: d.definedColumns(), Statements: map[string]string{}}, true)
	return d
}

// TryDefineView validates v without recording it in r.
func TryDefineView[T, S any](r *Registry[S], spec ViewSpec[T]) (*View[T, S], error) {
	if r == nil {
		return nil, errors.New("tabula.DefineView: registry is required")
	}
	if spec.Name == "" {
		return nil, errors.New(r.prefix + ".DefineView: name is required")
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf(r.prefix+".DefineView "+spec.Name+": "+format, args...)
	}
	rt := reflect.TypeFor[T]()
	if rt.Kind() != reflect.Struct {
		return nil, fail("row type %s is not a struct", rt)
	}
	v := &View[T, S]{descriptor: newDescriptor[T](r, spec.Name)}
	if err := walkFields(rt, nil, "", v.isNestedStruct, func(f reflect.StructField, idx []int, prefix, tag string) error {
		return v.tagged(f, idx, prefix, tag, fail)
	}, fail); err != nil {
		return nil, err
	}
	for _, c := range spec.Columns {
		c.once = false // Insert-only has no meaning on a projection.
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
	return v, nil
}

func (v *View[T, S]) isNestedStruct(ft reflect.Type) bool {
	if ft.Kind() != reflect.Struct {
		return false
	}
	if ft == reflect.TypeFor[time.Time]() {
		return false
	}
	if scanner(reflect.PointerTo(ft)) {
		return false
	}
	v.reg.mu.RLock()
	_, hasType := v.reg.types[ft]
	_, hasZero := v.reg.zeros[ft]
	v.reg.mu.RUnlock()
	return !hasType && !hasZero
}

func (v *View[T, S]) tagged(f reflect.StructField, index []int, namePrefix, tag string, fail func(string, ...any) error) error {
	parts := strings.Split(tag, ",")
	rawName := parts[0]
	if rawName == "" {
		return fail("field %s: db tag has no column name", f.Name)
	}
	name := namePrefix + rawName
	options := map[string]bool{}
	for _, option := range parts[1:] {
		switch option {
		case "json", "zero", "array", "secret":
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
	field := fieldAccessor[T](reflect.TypeFor[T](), index)
	return v.add(column[T]{
		name:     name,
		secret:   options["secret"],
		field:    field,
		goType:   f.Type,
		nullable: networkType(f.Type) || f.Type.Kind() == reflect.Pointer || options["zero"] || options["array"],
		target:   func(row *T) any { return wrap(field(row)) },
		wrap:     wrap,
		prefix:   namePrefix,
	}, fail)
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

// SelectAs generates a JOIN SELECT list from prefix-to-table-alias mappings
// (e.g. aliases{"o_": "o", "c_": "c"} produces "o.id AS o_id, ..., c.id AS c_id").
// Columns whose prefix is not in aliases are emitted as their plain name.
func (v *View[T, S]) SelectAs(aliases map[string]string) string {
	out := make([]string, len(v.cols))
	for i, c := range v.cols {
		if alias := strings.TrimSpace(aliases[c.prefix]); alias != "" {
			if c.prefix != "" {
				out[i] = alias + "." + strings.TrimPrefix(c.name, c.prefix) + " AS " + c.name
			} else {
				out[i] = alias + "." + c.name
			}
			continue
		}
		out[i] = c.name
	}
	return strings.Join(out, ", ")
}

// Bind binds this projection to the scope of s.
func (v *View[T, S]) Bind(s Session[S]) *ViewRows[T, S] {
	return &ViewRows[T, S]{readBinding: bindReader(&v.descriptor, s)}
}

// ViewRows decodes hand-written SQL using the bound projection.
type ViewRows[T, S any] struct{ readBinding[T, S] }

// WithLogger returns a handle with a per-binding logger override.
func (r *ViewRows[T, S]) WithLogger(logger *slog.Logger) *ViewRows[T, S] {
	copy := *r
	copy.logger = logger
	return &copy
}

// One reads at most one row. A missing row returns nil, nil.
func (r *ViewRows[T, S]) One(ctx context.Context, query string, args ...any) (*T, error) {
	return r.one(ctx, "view", r.d.lookupMeta(query, "view"), query, args)
}

// Select decodes all rows of query.
func (r *ViewRows[T, S]) Select(ctx context.Context, query string, args ...any) ([]T, error) {
	return r.selectOp(ctx, "view", r.d.lookupMeta(query, "view"), query, args)
}

// Iter decodes rows as they arrive. Breaking the range releases the cursor.
func (r *ViewRows[T, S]) Iter(ctx context.Context, query string, args ...any) iter.Seq2[T, error] {
	return r.stream(ctx, "view_iter", r.d.lookupMeta(query, "view_iter"), query, args)
}

// Scan executes a scalar single-row query with the registry error policy.
func (r *ViewRows[T, S]) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	return r.scan(ctx, "scan", r.d.lookupMeta(query, "scan"), query, args, dest...)
}
