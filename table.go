package tabula

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Column is an escape-hatch column of a row type T (TableSpec.Columns): its name
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

// TableSpec declares one table of rows T keyed by K: the qualified
// name, the default order of List, optional SoftDelete config, and the row
// type's db tags, read once by Define.
// Tag options: key, serial, scope, deleted, version, once, json, zero, array, secret.
type TableSpec[T, K any] struct {
	// RelationScope declares how a typed query may access this physical table.
	// A tagged scope column implies ByScopeColumn when this field is zero.
	// An untagged table needs an explicit ByRLS or Global declaration for typed use.
	RelationScope RelationScope
	Name, Order   string
	Columns       []Column[T]
	SoftDelete    *SoftDelete
	// ReadOnly restricts this table to read operations only: no INSERT/UPDATE/DELETE
	// statements or write privileges are derived, and write methods return ErrReadOnly.
	ReadOnly bool
	// AllowDelete opts this table into physical DELETE statements (Delete / DeleteWhere)
	// and derives the table-level DELETE privilege for sqlcheck. Cannot combine with ReadOnly.
	AllowDelete bool
	// GroupBy whitelists declared columns for CountBy aggregates and records
	// per-column count_by_<col> statements in Definition for sqlcheck verification.
	GroupBy []string
	// Aggregates registers scalar function/column pairs for sqlcheck verification.
	Aggregates ScalarAggregates
	// WriteMode restricts derived SQL, privileges and bound operations. An explicit
	// mode excludes ReadOnly and AllowDelete and rejects hand-written SQL on Rows.
	WriteMode WriteMode
	// UpdateColumns limits WriteUpdateOnly to these columns, in descriptor order.
	// Nil selects every updatable column. A version column must be included.
	UpdateColumns []string
}

// Table is the compiled handle for a TableSpec. Its column bindings, SQL and
// access modes are fixed by Define.
type Table[T, K, S any] struct {
	descriptor[T, S]
	relationScope           RelationScope
	order                   string
	readOnly, allowDelete   bool
	mode                    WriteMode
	updateColumns           map[string]bool
	groupBy                 map[string]bool
	groupByOrder            []string
	keys                    []int
	keyBinders              []func(*K) any
	keyBuilder              func(*T) K
	scope, deleted, version int // column index, -1 when absent
	del                     deleteSpec
	serial                  bool
	stmt                    statements
}

type statements struct {
	columns, get, lock, insert, update, count, list string
	values, insertPrefix                            string
	keyWhere, unscopedKeyWhere                      string
	deleteStmt, unscopedDelete                      string
	unscopedGet, unscopedLock                       string
	insertColumns, setColumns, onceColumns          []int
}

type role string

const (
	plain       role = ""
	keyRole     role = "key"
	scopeRole   role = "scope"
	deletedRole role = "deleted"
	versionRole role = "version"
)

type column[T any] struct {
	name      string
	fieldName string
	prefix    string
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
	// StatementQueries records the owner of each composed-query variant and
	// registered scalar aggregate (for example, sum_rank belongs to sum:rank).
	StatementQueries map[string]string `json:",omitempty"`
	// StatementShapes holds verification metadata for each typed variant.
	StatementShapes map[string]StatementShape `json:",omitempty"`
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

// Define validates spec against r (panicking on declaration mistakes at package
// initialization), derives its statements and column privileges, and records
// it in r.
func Define[T, K, S any](r *Registry[S], spec TableSpec[T, K]) *Table[T, K, S] {
	d, err := TryDefine(r, spec)
	if err != nil {
		panic(err)
	}
	cols := d.definedColumns()
	stmts := make(map[string]string)
	if d.mode.allows(selectOp) {
		stmts["get"], stmts["count"], stmts["list"] = d.stmt.get, d.stmt.count, d.stmt.list
		if d.stmt.unscopedGet != d.stmt.get {
			stmts["unscoped_get"] = d.stmt.unscopedGet
		}
	}
	if !d.readOnly && d.mode.allows(updateOp) {
		stmts["update"], stmts["lock"] = d.stmt.update, d.stmt.lock
		if d.stmt.unscopedLock != d.stmt.lock {
			stmts["unscoped_lock"] = d.stmt.unscopedLock
		}
	}
	if !d.readOnly && d.mode.allows(insertOp) {
		stmts["insert"] = d.stmt.insert
	}
	if d.allowDelete {
		stmts["delete"] = d.stmt.deleteStmt
		if d.stmt.unscopedDelete != d.stmt.deleteStmt {
			stmts["unscoped_delete"] = d.stmt.unscopedDelete
		}
	}
	for _, col := range d.groupByOrder {
		stmts["count_by_"+col] = "SELECT " + col + ", count(*) FROM " + d.name + " WHERE true GROUP BY " + col
	}
	var owners map[string]string
	for fn, col := range spec.Aggregates.columns {
		if owners == nil {
			owners = make(map[string]string)
		}
		op := strings.ToLower(fn)
		key := op + "_" + col
		base := scalarSQL(fn, col, d.name)
		owner := op + ":" + col
		stmts[key] = base + d.liveFilter(d.scope >= 0, nil).Where()
		owners[key] = owner
		if d.scope >= 0 {
			stmts["unscoped_"+key] = base + d.liveFilter(false, nil).Where()
			owners["unscoped_"+key] = owner
		}
	}
	d.entry = r.record(Definition{
		Name: d.name, Columns: cols, Statements: stmts, StatementQueries: owners,
		Privileges: d.derivePrivileges(),
	}, false)
	return d
}

// TryDefine validates spec and returns a compiled table without registering it.
func TryDefine[T, K, S any](r *Registry[S], spec TableSpec[T, K]) (*Table[T, K, S], error) {
	if r == nil {
		return nil, errors.New("tabula.Define: registry is required")
	}
	if spec.Name == "" {
		return nil, errors.New(r.prefix + ".Define: name is required")
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf(r.prefix+".Define "+spec.Name+": "+format, args...)
	}
	if err := spec.validateWriteMode(fail); err != nil {
		return nil, err
	}
	if spec.AllowDelete && r.deny&DenyDelete != 0 {
		return nil, fmt.Errorf("%w: %s.Define %s: AllowDelete is denied by registry policy", ErrCapabilityDenied, r.prefix, spec.Name)
	}
	rt := reflect.TypeFor[T]()
	if rt.Kind() != reflect.Struct {
		return nil, fail("row type %s is not a struct", rt)
	}
	t := &Table[T, K, S]{
		descriptor: newDescriptor[T](r, spec.Name),
		order:      spec.Order, readOnly: spec.ReadOnly, allowDelete: spec.AllowDelete, mode: spec.WriteMode,
		scope: -1, deleted: -1, version: -1,
	}
	if err := walkFields(rt, nil, "", nil, func(f reflect.StructField, idx []int, _, tag string) error { return t.tagged(f, idx, tag, fail) }, fail); err != nil {
		return nil, err
	}
	for _, c := range spec.Columns {
		if err := t.escape(c, fail); err != nil {
			return nil, err
		}
	}
	if err := t.bindUpdateColumns(spec.UpdateColumns, fail); err != nil {
		return nil, err
	}
	if err := t.bindRelationScope(spec.RelationScope); err != nil {
		return nil, fail("%v", err)
	}
	if len(spec.GroupBy) > 0 {
		t.groupBy = make(map[string]bool, len(spec.GroupBy))
		for _, col := range spec.GroupBy {
			if _, ok := t.byName[col]; !ok {
				return nil, fail("GroupBy column %s is not declared", col)
			}
			if t.groupBy[col] {
				return nil, fail("GroupBy column %s declared twice", col)
			}
			t.groupBy[col] = true
			t.groupByOrder = append(t.groupByOrder, col)
		}
	}
	seenAggregates := make(map[string]bool)
	for fn, col := range spec.Aggregates.columns {
		if _, ok := t.byName[col]; !ok {
			return nil, fail("%s column %s is not declared", fn, col)
		}
		key := fn + ":" + col
		if seenAggregates[key] {
			return nil, fail("%s column %s declared twice", fn, col)
		}
		seenAggregates[key] = true
	}
	if err := t.bindKeys(fail); err != nil {
		return nil, err
	}
	if err := t.bindSoftDelete(spec.SoftDelete, fail); err != nil {
		return nil, err
	}
	t.derive()
	if t.mode == WriteUpdateOnly && len(t.stmt.setColumns) == 0 {
		return nil, fail("WriteUpdateOnly needs an updatable column")
	}
	return t, nil
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
		case "key", "scope", "deleted", "version":
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
	if t.reg.scopeType != nil && f.Type == t.reg.scopeType && r != scopeRole {
		return fail("column %s: %s maps only the scope column", name, t.reg.scopeType)
	}
	wrap, err := t.reg.resolve(f.Type, options["json"], options["zero"], options["array"])
	if err != nil {
		return fail("column %s: %w", name, err)
	}
	t.serial = t.serial || options["serial"]
	field := fieldAccessor[T](reflect.TypeFor[T](), index)
	return t.add(column[T]{
		name: name, fieldName: f.Name, role: r, once: options["once"], secret: options["secret"], field: field,
		goType: f.Type, nullable: networkType(f.Type) || f.Type.Kind() == reflect.Pointer || options["zero"] || options["array"],
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
	case scopeRole:
		if ft != t.reg.scopeType {
			return fail("scope column %s must be %s", name, t.reg.scopeType)
		}
		slot = &t.scope
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
		if t.mode.allows(updateOp) && c.updatable() && t.permitsColumn(c.name) {
			t.stmt.setColumns = append(t.stmt.setColumns, i)
		}
		if c.once && t.mode.allows(updateOp) {
			t.stmt.onceColumns = append(t.stmt.onceColumns, i)
		}
		skipSerial := t.serial && len(t.keys) == 1 && i == t.keys[0]
		skipDeleted := i == t.deleted && t.del.excludeIns
		if t.mode.allows(insertOp) && !skipSerial && !skipDeleted {
			t.stmt.insertColumns = append(t.stmt.insertColumns, i)
		}
	}
	t.stmt.columns = strings.Join(names, ", ")
	if t.mode.allows(insertOp) {
		t.deriveInsert()
	}
	if !t.mode.allows(selectOp) {
		return
	}

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
	if t.scope >= 0 {
		where = byKey + " AND " + names[t.scope] + " = $" + strconv.Itoa(next) + alive
		next++
	}
	t.stmt.keyWhere, t.stmt.unscopedKeyWhere = where, byKey+alive
	t.stmt.get = "SELECT " + t.stmt.columns + " FROM " + t.name + where
	t.stmt.lock = t.stmt.get + " FOR UPDATE"
	t.stmt.unscopedGet = "SELECT " + t.stmt.columns + " FROM " + t.name + byKey + alive
	t.stmt.unscopedLock = t.stmt.unscopedGet + " FOR UPDATE"

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
	t.stmt.update = "UPDATE " + t.name + " SET " + strings.Join(sets, ", ") + where + cas
	t.stmt.deleteStmt = "DELETE FROM " + t.name + where
	t.stmt.unscopedDelete = "DELETE FROM " + t.name + byKey + alive
	t.stmt.count = "SELECT count(*) FROM " + t.name + " WHERE true"
	t.stmt.list = "SELECT " + t.stmt.columns + " FROM " + t.name + " WHERE true"
}

func (t *Table[T, K, S]) deriveInsert() {
	names := t.InsertColumns()
	switch {
	case len(names) > 0:
		holders := make([]string, len(names))
		for i := range holders {
			holders[i] = "$" + strconv.Itoa(i+1)
		}
		t.stmt.insertPrefix = "INSERT INTO " + t.name + " (" + strings.Join(names, ", ") + ") VALUES "
		t.stmt.values = t.stmt.insertPrefix + "(" + strings.Join(holders, ", ") + ")"
	case t.mode == WriteInsertOnly:
		// Name the serial key so the required column grant is determinate.
		t.stmt.values = "INSERT INTO " + t.name + " (" + t.cols[t.keys[0]].name + ") VALUES (DEFAULT)"
	default:
		t.stmt.values = "INSERT INTO " + t.name + " DEFAULT VALUES"
	}
	t.stmt.insert = t.stmt.values
	if t.serial && t.mode.allows(selectOp) {
		t.stmt.insert += " RETURNING " + t.cols[t.keys[0]].name
	}
}

// Cols is the select list, for hand-written statements scanned with Rows.Select.
func (t *Table[T, K, S]) Cols() string { return t.stmt.columns }

// Insert is the INSERT base without RETURNING, for hand-written variants.
// WriteUpdateOnly has no INSERT base and returns an empty string.
func (t *Table[T, K, S]) Insert() string { return t.stmt.values }

// InsertColumns returns the columns bound from each input row for INSERT and COPY, in bind order.
func (t *Table[T, K, S]) InsertColumns() []string {
	out := make([]string, len(t.stmt.insertColumns))
	for i, colIdx := range t.stmt.insertColumns {
		out[i] = t.cols[colIdx].name
	}
	return out
}

// Bind binds the table to a transaction session.
func (t *Table[T, K, S]) Bind(s Session[S]) *Rows[T, K, S] {
	r := &Rows[T, K, S]{t: t, readBinding: bindReader(&t.descriptor, s)}
	if len(t.stmt.onceColumns) > 0 {
		r.locks = &lockedRows{}
	}
	return r
}

// ReadOnly reports whether the declaration permits only reads.
func (t *Table[T, K, S]) ReadOnly() bool { return t.readOnly }

// AllowsDelete reports whether the table permits physical deletes.
func (t *Table[T, K, S]) AllowsDelete() bool { return t.allowDelete }
