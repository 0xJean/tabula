package tabula

import (
	"fmt"
	"reflect"
	"time"
)

// TypeDefault declares a registry's exact PostgreSQL type for a Go value type.
// Construct it with TypeMapping.
type TypeDefault struct {
	kind catalogType
	typ  any // original PGType[V], including its codec and input nullability
}

// TypeMapping retains the catalog identity and input codec of kind as a
// registry default for V. A nullable *V field can use the same declaration.
func TypeMapping[V any](kind PGType[V]) TypeDefault {
	return TypeDefault{kind: kind.id, typ: kind}
}

func registryTypeDefaults(defaults []TypeDefault) map[reflect.Type]TypeDefault {
	if len(defaults) == 0 {
		return nil
	}
	types := make(map[reflect.Type]TypeDefault, len(defaults))
	for _, d := range defaults {
		t := d.kind.goType
		if t == nil {
			panic("tabula.NewRegistry: DefaultTypes requires declared PostgreSQL types")
		}
		if _, ok := types[t]; ok {
			panic(fmt.Sprintf("tabula.NewRegistry: duplicate default type for %s", t))
		}
		types[t] = d
	}
	return types
}

// ColumnMapping overrides one table column's type. Construct it with ColumnType.
type ColumnMapping struct {
	name string
	typ  TypeDefault
}

// ColumnType declares the exact PostgreSQL type and input codec of a column.
// Its Go type must match the field or the element of a pointer field.
func ColumnType[V any](name string, kind PGType[V]) ColumnMapping {
	return ColumnMapping{name: name, typ: TypeMapping(kind)}
}

func (col column[T]) defaultType(defaults map[reflect.Type]TypeDefault) TypeDefault {
	t := col.goType
	if d, ok := defaults[t]; ok {
		return d
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
		if d, ok := defaults[t]; ok {
			return d
		}
	}
	if col.needsColumnType {
		return TypeDefault{}
	}
	switch t {
	case reflect.TypeFor[bool]():
		return TypeMapping(Bool())
	case reflect.TypeFor[int16]():
		return TypeMapping(CatalogType[int16]("pg_catalog", "int2", nil))
	case reflect.TypeFor[int32]():
		return TypeMapping(Int4())
	case reflect.TypeFor[int64]():
		return TypeMapping(Int8())
	case reflect.TypeFor[float32]():
		return TypeMapping(CatalogType[float32]("pg_catalog", "float4", nil))
	case reflect.TypeFor[float64]():
		return TypeMapping(CatalogType[float64]("pg_catalog", "float8", nil))
	case reflect.TypeFor[string]():
		return TypeMapping(Text())
	case reflect.TypeFor[[]byte]():
		return TypeMapping(CatalogType[[]byte]("pg_catalog", "bytea", nil))
	case reflect.TypeFor[time.Time]():
		return TypeMapping(Timestamptz())
	default:
		return TypeDefault{}
	}
}

func (col column[T]) typedRef(src *occurrence, kind catalogType) *fieldRef {
	return &fieldRef{source: src, name: col.name, kind: kind, nullable: col.nullable, secret: col.secret}
}

// TableLayout holds the column types and scan targets of one registered table.
// Multiple aliases share the immutable layout and have distinct occurrences.
type TableLayout[T, S any] struct {
	owner *descriptor[T, S]
	types []TypeDefault
	as    func(string) RowSource[T, S]
}

// MapTable declares a layout for a registered physical table. It panics on
// invalid declarations. Mapping or aliasing a table registers no statements.
func MapTable[T, K, S any](table *Table[T, K, S], overrides ...ColumnMapping) *TableLayout[T, S] {
	layout, err := TryMapTable(table, overrides...)
	if err != nil {
		panic(err)
	}
	return layout
}

// TryMapTable checks column overrides and resolves conventional types. Fields
// without a mapping can be omitted from a projection. A scope or soft-delete
// field used by the source must have a mapping.
func TryMapTable[T, K, S any](table *Table[T, K, S], overrides ...ColumnMapping) (*TableLayout[T, S], error) {
	if table == nil || table.entry == nil || table.relationScope.kind == 0 {
		return nil, fmt.Errorf("tabula.MapTable: a registered physical table with a declared scope is required")
	}
	if _, err := parsePhysicalName(table.name); err != nil {
		return nil, err
	}
	types := make([]TypeDefault, len(table.cols))
	for _, o := range overrides {
		i, ok := table.byName[o.name]
		if !ok || !typedIdent(o.name) {
			return nil, fmt.Errorf("tabula.MapTable: unknown column %q", o.name)
		}
		if types[i].kind.goType != nil {
			return nil, fmt.Errorf("tabula.MapTable: duplicate type for column %q", o.name)
		}
		field, value := table.cols[i].goType, o.typ.kind.goType
		if !matchesColumnType(field, value) {
			return nil, fmt.Errorf("tabula.MapTable: column %q holds %s, declared %v", o.name, field, value)
		}
		types[i] = o.typ
	}
	var special []int
	for i, col := range table.cols {
		if types[i].kind.goType == nil {
			types[i] = col.defaultType(table.reg.defaultTypes)
		}
		if i == table.deleted || i == table.scope && table.relationScope == ByScopeColumn() {
			if !typedIdent(col.name) || types[i].kind.goType == nil {
				return nil, fmt.Errorf("tabula.MapTable: declare a PostgreSQL type for column %q", col.name)
			}
			special = append(special, i)
		}
	}
	var live rowLivePolicy
	if table.deleted >= 0 {
		var err error
		live, err = table.del.rowLivePolicy(types[table.deleted].kind)
		if err != nil {
			return nil, err
		}
	}
	as := func(alias string) RowSource[T, S] {
		rel := FromTable(table, alias, table.relationScope)
		columns := make([]RowColumn, len(special))
		for j, i := range special {
			columns[j] = RowColumn{ref: table.cols[i].typedRef(rel.source, types[i].kind)}
		}
		src := newRowSource(table, rel, columns, &live)
		src.types = types
		return src
	}
	return &TableLayout[T, S]{owner: &table.descriptor, types: types, as: as}, nil
}

// As creates a fresh occurrence using the table's declared scope and live-row
// policy. Reuse the returned source for fields, projections and joins.
func (layout *TableLayout[T, S]) As(alias string) RowSource[T, S] {
	if layout == nil || layout.as == nil {
		panic("tabula.TableLayout.As: a declared layout is required")
	}
	return layout.as(alias)
}

// TableField identifies a typed column of one registered table across aliases.
type TableField[T, V any] struct {
	issuer *statementIssuer
	entry  *registration
	col    *column[T]
	kind   PGType[V]
}

// MappedColumn resolves a layout field once. V must match its declared value
// type. A pointer field mapped through its element is referenced with that
// element type and retains its SQL nullability.
func MappedColumn[V, T, S any](layout *TableLayout[T, S], name string) TableField[T, V] {
	if layout == nil || layout.owner == nil {
		panic("tabula.MappedColumn: a declared layout is required")
	}
	i, ok := layout.owner.byName[name]
	if !ok || !typedIdent(name) {
		panic(fmt.Sprintf("tabula.MappedColumn: unknown column %q", name))
	}
	d := layout.types[i]
	kind, ok := d.typ.(PGType[V])
	if !ok {
		panic(fmt.Sprintf("tabula.MappedColumn: column %q is mapped to %v, requested %s", name, d.kind.goType, reflect.TypeFor[V]()))
	}
	return TableField[T, V]{issuer: layout.owner.reg.statementIssuer, entry: layout.owner.entry,
		col: &layout.owner.cols[i], kind: kind}
}

// On selects this column on an occurrence of the same registered table.
func (f TableField[T, V]) On(rel Rel[T]) Field[V] {
	if rel.source == nil || rel.source.entry != f.entry || rel.source.issuer != f.issuer {
		panic("tabula.TableField.On: the occurrence must belong to the mapped table")
	}
	return Field[V]{ref: f.col.typedRef(rel.source, f.kind.id), kind: f.kind}
}

// Input creates a distinct input with the field's declared type, codec and
// secret flag, including when its expression is used on another relation.
func (f TableField[T, V]) Input(name string) Slot[V] {
	slot := Input(name, f.kind)
	slot.input.secret = f.col.secret
	return slot
}
