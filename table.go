package tabula

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Column is an escape-hatch column of a row type T (Table.Columns): its name
// and the address of the value that holds it, for a column the db tags do not
// declare (a legacy conversion, a generated column, or an array wrapper).
type Column[T any] struct {
	Name   string
	Addr   func(*T) any
	once   bool
	secret bool
}

// Col declares an escape-hatch column. A field with a db tag cannot also be
// declared here: tag it db:"-" first.
func Col[T any](name string, addr func(*T) any) Column[T] { return Column[T]{Name: name, Addr: addr} }

// Once marks a column as written on insert only.
func (c Column[T]) Once() Column[T] { c.once = true; return c }

// Secret marks a column's values as redacted in statement logs even when
// LogConfig.Values is ValuesAllowed.
func (c Column[T]) Secret() Column[T] { c.secret = true; return c }

// Table describes one table of rows T keyed by K under scope S: the qualified
// name, the default order of List, optional SoftDelete config, and the row
// type's db tags, read once by Define.
// Tag options: key, serial, tenant, deleted, version, once, json, zero, array, secret.
type Table[T, K, S any] struct {
	Name, Order string
	Columns     []Column[T]
	SoftDelete  *SoftDelete

	reg                      *Registry[S]
	cols                     []column[T]
	byName                   map[string]int
	keys                     []int
	keyBinders               []func(*K) any
	keyBuilder               func(*T) K
	tenant, deleted, version int // column index, -1 when absent
	del                      deleteSpec
	serial                   bool
	stmt                     statements
	defID                    int // 1-based index in reg.defs; 0 when undefined
	meta                     *stmtMetaStore
}

type statements struct {
	columns, get, lock, insert, update, count, list string
	values                                          string
	platformGet, platformLock                       string
	insertColumns, setColumns, onceColumns          []int
}

type role string

const (
	plain       role = ""
	keyRole     role = "key"
	tenantRole  role = "tenant"
	deletedRole role = "deleted"
	versionRole role = "version"
)

type column[T any] struct {
	name      string
	fieldName string
	role      role
	once      bool
	secret    bool
	goType    reflect.Type
	nullable  bool
	target    func(*T) any
	field     func(*T) any
	wrap      func(any) any
}

// Definition is one defined table as Registry.Defined lists it.
type Definition struct {
	Name       string
	Columns    []DefinedColumn
	Statements map[string]string
	Privileges []DefinedPrivilege
}

// DefinedColumn is one column's metadata for schema and privilege checks.
type DefinedColumn struct {
	Name     string
	GoType   string
	Nullable bool
	Role     string
	Once     bool
	Secret   bool
}

// Define validates t against r (panicking on declaration mistakes at package
// initialization), derives its statements and column privileges, and records
// it in r.
func Define[T, K, S any](r *Registry[S], t Table[T, K, S]) *Table[T, K, S] {
	d, err := TryDefine(r, t)
	if err != nil {
		panic(err)
	}
	cols := make([]DefinedColumn, len(d.cols))
	for i, c := range d.cols {
		cols[i] = DefinedColumn{Name: c.name, GoType: c.goType.String(), Nullable: c.nullable, Role: string(c.role), Once: c.once, Secret: c.secret}
	}
	stmts := map[string]string{
		"get": d.stmt.get, "lock": d.stmt.lock, "insert": d.stmt.insert,
		"update": d.stmt.update, "count": d.stmt.count, "list": d.stmt.list,
	}
	if d.stmt.platformGet != d.stmt.get {
		stmts["platform_get"], stmts["platform_lock"] = d.stmt.platformGet, d.stmt.platformLock
	}
	d.defID = r.record(d.Name, cols, stmts, derivePrivileges(d.Name, cols, d.stmt))
	return d
}

// TryDefine validates and derives statements for t without recording it in r.
func TryDefine[T, K, S any](r *Registry[S], t Table[T, K, S]) (*Table[T, K, S], error) {
	if r == nil {
		return nil, errors.New("tabula.Define: registry is required")
	}
	if t.Name == "" {
		return nil, errors.New(r.prefix + ".Define: name is required")
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf(r.prefix+".Define "+t.Name+": "+format, args...)
	}
	rt := reflect.TypeFor[T]()
	if rt.Kind() != reflect.Struct {
		return nil, fail("row type %s is not a struct", rt)
	}
	t.reg = r
	t.cols, t.byName, t.keys = nil, map[string]int{}, nil
	t.tenant, t.deleted, t.version, t.serial, t.defID = -1, -1, -1, false, 0
	t.del = deleteSpec{}
	t.meta = newStmtMetaStore()
	if err := t.fields(rt, nil, true, fail); err != nil {
		return nil, err
	}
	for _, c := range t.Columns {
		if err := t.escape(c, fail); err != nil {
			return nil, err
		}
	}
	if err := t.bindKeys(fail); err != nil {
		return nil, err
	}
	if err := t.bindSoftDelete(fail); err != nil {
		return nil, err
	}
	t.derive()
	return &t, nil
}

func (t *Table[T, K, S]) fields(rt reflect.Type, prefix []int, top bool, fail func(string, ...any) error) error {
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
			if err := t.fields(f.Type, index, false, fail); err != nil {
				return err
			}
			continue
		case !tagged:
			return fail(`exported field %s has no db tag (use db:"-" to skip)`, f.Name)
		}
		if err := t.tagged(f, index, tag, fail); err != nil {
			return err
		}
	}
	return nil
}

type parsedTag struct {
	name    string
	role    role
	options map[string]bool
}

func parseColumnTag(tag string) (parsedTag, error) {
	parts := strings.Split(tag, ",")
	name := parts[0]
	if name == "" {
		return parsedTag{}, errors.New("db tag has no column name")
	}
	var roles []role
	options := map[string]bool{}
	for _, option := range parts[1:] {
		switch option {
		case "key", "tenant", "deleted", "version":
			roles = append(roles, role(option))
		case "serial", "once", "json", "zero", "array", "secret":
		default:
			return parsedTag{}, fmt.Errorf("column %s: option %s is unknown", name, option)
		}
		options[option] = true
	}
	r := plain
	if len(roles) > 0 {
		r = roles[0]
	}
	switch {
	case len(roles) > 1:
		return parsedTag{}, fmt.Errorf("column %s: option %s cannot combine with %s", name, roles[0], roles[1])
	case options["json"] && options["zero"]:
		return parsedTag{}, fmt.Errorf("column %s: option json cannot combine with zero", name)
	case options["array"] && options["json"]:
		return parsedTag{}, fmt.Errorf("column %s: option array cannot combine with json", name)
	case options["array"] && options["zero"]:
		return parsedTag{}, fmt.Errorf("column %s: option array cannot combine with zero", name)
	}
	for _, option := range []string{"once", "json", "zero", "array", "secret"} {
		if r != plain && options[option] {
			return parsedTag{}, fmt.Errorf("column %s: option %s cannot combine with %s", name, option, r)
		}
	}
	return parsedTag{name: name, role: r, options: options}, nil
}

func (t *Table[T, K, S]) tagged(f reflect.StructField, index []int, tag string, fail func(string, ...any) error) error {
	parsed, err := parseColumnTag(tag)
	if err != nil {
		if parsed.name == "" && strings.HasPrefix(err.Error(), "db tag") {
			return fail("field %s: %s", f.Name, err.Error())
		}
		return fail("%s", err.Error())
	}
	name, r, options := parsed.name, parsed.role, parsed.options
	if options["serial"] && (r != keyRole || !integer(f.Type)) {
		return fail("serial requires an integer key column")
	}
	if err := t.role(name, r, f.Type, fail); err != nil {
		return err
	}
	if t.reg.tenantType != nil && f.Type == t.reg.tenantType && r != tenantRole {
		return fail("column %s: %s maps only the tenant column", name, t.reg.tenantType)
	}
	wrap, err := t.reg.resolve(f.Type, options["json"], options["zero"], options["array"])
	if err != nil {
		return fail("column %s: %w", name, err)
	}
	t.serial = t.serial || options["serial"]
	field := fieldAccessor[T](reflect.TypeFor[T](), index)
	return t.add(column[T]{
		name: name, fieldName: f.Name, role: r, once: options["once"], secret: options["secret"], field: field,
		goType: f.Type, nullable: f.Type.Kind() == reflect.Pointer || options["zero"] || options["array"],
		target: func(row *T) any { return wrap(field(row)) }, wrap: wrap,
	}, fail)
}

func (t *Table[T, K, S]) role(name string, r role, ft reflect.Type, fail func(string, ...any) error) error {
	var slot *int
	switch r {
	case plain:
		return nil
	case keyRole:
		t.keys = append(t.keys, len(t.cols))
		return nil
	case tenantRole:
		if ft != t.reg.tenantType {
			return fail("tenant column %s must be %s", name, t.reg.tenantType)
		}
		slot = &t.tenant
	case deletedRole:
		slot = &t.deleted
	case versionRole:
		if !integer(ft) {
			return fail("version column %s must be an integer", name)
		}
		slot = &t.version
	}
	if *slot >= 0 {
		return fail("%s column declared twice", r)
	}
	*slot = len(t.cols)
	return nil
}

func (t *Table[T, K, S]) escape(c Column[T], fail func(string, ...any) error) error {
	if c.Name == "" || c.Addr == nil {
		return fail("Col %q needs a name and an address", c.Name)
	}
	target := c.Addr
	vt := reflect.TypeOf(c.Addr(new(T)))
	if !scanner(vt) {
		if vt == nil || vt.Kind() != reflect.Pointer {
			return fail("column %s: Col must address a field, got %v", c.Name, vt)
		}
		wrap, err := t.reg.Resolve(vt.Elem(), false, false)
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
	return t.add(column[T]{name: c.Name, once: c.once, secret: c.secret, target: target, goType: held, nullable: nullable}, fail)
}

func (t *Table[T, K, S]) add(c column[T], fail func(string, ...any) error) error {
	if _, ok := t.byName[c.name]; ok {
		return fail("column %s declared twice", c.name)
	}
	t.byName[c.name] = len(t.cols)
	t.cols = append(t.cols, c)
	return nil
}

func integer(ft reflect.Type) bool {
	switch ft.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

func (t *Table[T, K, S]) derive() {
	names := make([]string, len(t.cols))
	for i, c := range t.cols {
		names[i] = c.name
		if c.role != keyRole && c.role != tenantRole && !c.once {
			t.stmt.setColumns = append(t.stmt.setColumns, i)
		}
		if c.once {
			t.stmt.onceColumns = append(t.stmt.onceColumns, i)
		}
		skipSerial := t.serial && len(t.keys) == 1 && i == t.keys[0]
		skipDeleted := i == t.deleted && t.del.excludeIns
		if !skipSerial && !skipDeleted {
			t.stmt.insertColumns = append(t.stmt.insertColumns, i)
		}
	}
	t.stmt.columns = strings.Join(names, ", ")

	keyParts := make([]string, len(t.keys))
	for pos, keyIdx := range t.keys {
		keyParts[pos] = names[keyIdx] + " = $" + strconv.Itoa(pos+1)
	}
	byKey := " WHERE " + strings.Join(keyParts, " AND ")
	alive := ""
	if t.deleted >= 0 {
		alive = " AND " + t.del.aliveSQL
	}
	next := len(t.keys) + 1
	where := byKey + alive
	if t.tenant >= 0 {
		where = byKey + " AND " + names[t.tenant] + " = $" + strconv.Itoa(next) + alive
		next++
	}
	t.stmt.get = "SELECT " + t.stmt.columns + " FROM " + t.Name + where
	t.stmt.lock = t.stmt.get + " FOR UPDATE"
	t.stmt.platformGet = "SELECT " + t.stmt.columns + " FROM " + t.Name + byKey + alive
	t.stmt.platformLock = t.stmt.platformGet + " FOR UPDATE"

	insertNames, holders := make([]string, 0, len(names)), make([]string, 0, len(names))
	for _, i := range t.stmt.insertColumns {
		insertNames = append(insertNames, names[i])
		holders = append(holders, "$"+strconv.Itoa(len(holders)+1))
	}
	t.stmt.values = "INSERT INTO " + t.Name + " (" + strings.Join(insertNames, ", ") + ") VALUES (" + strings.Join(holders, ", ") + ")"
	t.stmt.insert = t.stmt.values
	if t.serial {
		t.stmt.insert += " RETURNING " + names[t.keys[0]]
	}

	sets := make([]string, 0, len(t.stmt.setColumns))
	cas := ""
	for _, i := range t.stmt.setColumns {
		holder := "$" + strconv.Itoa(next)
		sets = append(sets, names[i]+" = "+holder)
		if i == t.version {
			cas = " AND " + names[i] + " = " + holder + " - 1"
		}
		next++
	}
	t.stmt.update = "UPDATE " + t.Name + " SET " + strings.Join(sets, ", ") + where + cas
	t.stmt.count = "SELECT count(*) FROM " + t.Name + " WHERE true"
	t.stmt.list = "SELECT " + t.stmt.columns + " FROM " + t.Name + " WHERE true"
}

func (t *Table[T, K, S]) prefix() string {
	if t.reg != nil {
		return t.reg.prefix
	}
	return "tabula"
}

// Cols is the select list, for hand-written statements scanned with Rows.Select.
func (t *Table[T, K, S]) Cols() string { return t.stmt.columns }

// Insert is the INSERT base without RETURNING, for hand-written variants.
func (t *Table[T, K, S]) Insert() string { return t.stmt.values }

// InsertColumns returns the column names included in Table.Insert() in bind order.
func (t *Table[T, K, S]) InsertColumns() []string {
	out := make([]string, len(t.stmt.insertColumns))
	for i, colIdx := range t.stmt.insertColumns {
		out[i] = t.cols[colIdx].name
	}
	return out
}

// Bind binds the table to a transaction session.
func (t *Table[T, K, S]) Bind(s Session[S]) *Rows[T, K, S] { return &Rows[T, K, S]{t: t, s: s} }

// Statement records a hand-written statement under key on this table's own
// Definition. Optional StatementOptions (Quiet, Secret) configure logging for
// this statement. Declaring a key twice or calling on an undefined table panics.
func (t *Table[T, K, S]) Statement(key, sql string, opts ...StatementOption) string {
	if t.reg == nil || t.defID <= 0 {
		panic(t.prefix() + ".Statement " + t.Name + ": table is not defined")
	}
	t.reg.mu.Lock()
	if t.defID > len(t.reg.defs) {
		t.reg.mu.Unlock()
		panic(t.prefix() + ".Statement " + t.Name + ": table is not defined")
	}
	d := &t.reg.defs[t.defID-1]
	if _, ok := d.Statements[key]; ok {
		t.reg.mu.Unlock()
		panic(fmt.Sprintf("%s.Statement %s: key %s declared twice", t.prefix(), t.Name, key))
	}
	d.Statements[key] = sql
	t.reg.mu.Unlock()

	meta := stmtMeta{key: t.Name + "." + key}
	for _, opt := range opts {
		if opt != nil {
			opt(&meta)
		}
	}
	if t.meta != nil {
		t.meta.put(sql, meta)
	}
	return sql
}

func (t *Table[T, K, S]) lookupMeta(query, fallbackAction string) stmtMeta {
	return t.meta.get(query, t.Name+"."+fallbackAction)
}

// Definition returns a deep copy of this table's own registered Definition.
func (t *Table[T, K, S]) Definition() Definition {
	if t.reg == nil || t.defID <= 0 {
		panic(t.prefix() + ".Definition " + t.Name + ": table is not defined")
	}
	t.reg.mu.RLock()
	defer t.reg.mu.RUnlock()
	if t.defID > len(t.reg.defs) {
		panic(t.prefix() + ".Definition " + t.Name + ": table is not defined")
	}
	return cloneDefinition(t.reg.defs[t.defID-1])
}
