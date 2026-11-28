package tabula

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/0xJean/tabula/internal/planrt"
)

// descriptor contains the compiled column set and a reference to its registry entry.
type descriptor[T, S any] struct {
	reg       *Registry[S]
	name      string
	cols      []column[T]
	byName    map[string]int
	entry     *registration
	fixed     map[string]*planrt.CompiledShape
	plansOnly bool
}

func newDescriptor[T, S any](reg *Registry[S], name string) descriptor[T, S] {
	return descriptor[T, S]{reg: reg, name: name, byName: make(map[string]int)}
}

// Name returns the name supplied at definition time.
func (d *descriptor[T, S]) Name() string { return d.name }
func (d *descriptor[T, S]) prefix() string {
	if d.reg == nil {
		return "tabula"
	}
	return d.reg.prefix
}

func (d *descriptor[T, S]) definedColumns() []DefinedColumn {
	cols := make([]DefinedColumn, len(d.cols))
	for i, c := range d.cols {
		cols[i] = DefinedColumn{Name: c.name, GoType: c.goType.String(), Nullable: c.nullable, Role: string(c.role), Once: c.once, Secret: c.secret}
	}
	return cols
}

func (d *descriptor[T, S]) registered() *registration {
	if d.entry == nil {
		panic(d.prefix() + ": descriptor " + d.name + " is not registered")
	}
	return d.entry
}

// Definition returns a deep copy of the current registry entry.
func (d *descriptor[T, S]) Definition() Definition {
	e := d.registered()
	d.reg.mu.RLock()
	defer d.reg.mu.RUnlock()
	return cloneDefinition(e.def)
}

// Statement registers hand-written SQL for verification and observation.
// Keys are unique within a table or view.
func (d *descriptor[T, S]) Statement(key, query string, opts ...StatementOption) string {
	if d.plansOnly {
		panic(ErrOperationDenied)
	}
	if d.reg != nil && d.reg.deny&DenyLegacySQL != 0 {
		panic(ErrLegacySQL)
	}
	return d.registerStatement(key, query, "", opts)
}

func (d *descriptor[T, S]) registerStatement(key, query, queryKey string, opts []StatementOption) string {
	e := d.registered()
	meta := statementMeta(d.name+"."+key, opts)
	d.reg.mu.Lock()
	defer d.reg.mu.Unlock()
	if queryKey != "" {
		for shapeKey := range e.def.StatementShapes {
			if e.def.StatementQueries[shapeKey] == queryKey {
				panic(fmt.Sprintf("%s: %s plan %q already registered", d.prefix(), d.name, queryKey))
			}
		}
	}
	if _, ok := e.def.Statements[key]; ok {
		panic(fmt.Sprintf("%s.Statement %s: key %s declared twice", d.prefix(), d.name, key))
	}
	e.def.Statements[key] = query
	if queryKey != "" {
		if e.def.StatementQueries == nil {
			e.def.StatementQueries = make(map[string]string)
		}
		e.def.StatementQueries[key] = queryKey
	}
	e.bySQL[query] = meta
	return query
}

func statementMeta(key string, opts []StatementOption) stmtMeta {
	m := stmtMeta{key: key}
	for _, opt := range opts {
		if opt != nil {
			opt(&m)
		}
	}
	return m
}

func (d *descriptor[T, S]) lookupMeta(query, action string) stmtMeta {
	if d.entry != nil {
		d.reg.mu.RLock()
		m, ok := d.entry.bySQL[query]
		d.reg.mu.RUnlock()
		if ok {
			return m
		}
	}
	return stmtMeta{key: d.name + "." + action}
}

func (d *descriptor[T, S]) require(p DefinedPrivilege, err error) {
	if d.plansOnly {
		panic(ErrOperationDenied)
	}
	if err != nil {
		panic(fmt.Sprintf("%s.Requires %s: %v", d.prefix(), d.name, err))
	}
	e := d.registered()
	d.reg.mu.Lock()
	defer d.reg.mu.Unlock()
	e.def.Privileges = append(e.def.Privileges, p)
}

func walkFields(
	rt reflect.Type, prefixIdx []int, namePrefix string,
	nested func(reflect.Type) bool, visit func(reflect.StructField, []int, string, string) error,
	fail func(string, ...any) error,
) error {
	for i := range rt.NumField() {
		f := rt.Field(i)
		index := append(slices.Clone(prefixIdx), i)
		tag, tagged := f.Tag.Lookup("db")
		if tag == "-" {
			continue
		}
		if !f.IsExported() {
			if !tagged && f.Anonymous && f.Type.Kind() == reflect.Struct {
				if err := walkFields(f.Type, index, namePrefix, nested, visit, fail); err != nil {
					return err
				}
			}
			continue
		}
		switch {
		case !tagged && f.Anonymous && f.Type.Kind() == reflect.Struct:
			if err := walkFields(f.Type, index, namePrefix, nested, visit, fail); err != nil {
				return err
			}
			continue
		case !tagged:
			return fail(`exported field %s has no db tag (use db:"-" to skip)`, f.Name)
		}
		// Nested struct prefix: e.g. `Job JobRow` with `db:"j_"` expands
		// JobRow's exported fields under prefix "j_" (j_id, j_code, ...).
		if nested != nil && !strings.Contains(tag, ",") && nested(f.Type) {
			if err := walkFields(f.Type, index, namePrefix+tag, nested, visit, fail); err != nil {
				return err
			}
			continue
		}
		if err := visit(f, index, namePrefix, tag); err != nil {
			return err
		}
	}
	return nil
}

func (d *descriptor[T, S]) escape(c Column[T], fail func(string, ...any) error) error {
	if c.Name == "" || c.Addr == nil {
		return fail("Col %q needs a name and an address", c.Name)
	}
	target := c.Addr
	vt := reflect.TypeOf(c.Addr(new(T)))
	if !scanner(vt) {
		if vt == nil || vt.Kind() != reflect.Pointer {
			return fail("column %s: Col must address a field, got %v", c.Name, vt)
		}
		wrap, err := d.reg.Resolve(vt.Elem(), false, false)
		if err != nil {
			return fail("column %s: %w", c.Name, err)
		}
		target = func(row *T) any { return wrap(c.Addr(row)) }
	}
	held := vt
	if held.Kind() == reflect.Pointer {
		held = held.Elem()
	}
	nullable := networkType(held) || held.Kind() == reflect.Pointer || strings.HasPrefix(held.Name(), "Null")
	return d.add(column[T]{name: c.Name, once: c.once, secret: c.secret, target: target, goType: held, nullable: nullable}, fail)
}

func (d *descriptor[T, S]) add(c column[T], fail func(string, ...any) error) error {
	if _, ok := d.byName[c.name]; ok {
		return fail("column %s declared twice", c.name)
	}
	d.byName[c.name] = len(d.cols)
	d.cols = append(d.cols, c)
	return nil
}
