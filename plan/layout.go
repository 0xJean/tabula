package plan

import (
	"fmt"
	"reflect"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/internal/planrt"
)

// ColumnMapping overrides one table column's type. Construct it with ColumnType.
type ColumnMapping struct {
	name string
	typ  planrt.DefaultType
}

// ColumnType declares the exact PostgreSQL type and input codec of a column.
// Its Go type must match the field or the element of a pointer field.
func ColumnType[V any](name string, kind tabula.PGType[V]) ColumnMapping {
	return ColumnMapping{name: name, typ: defaultInfo(tabula.TypeMapping(kind))}
}

func columnDefaultType[T any](col column[T], defaults map[reflect.Type]planrt.DefaultType) planrt.DefaultType {
	t := col.GoType
	if d, ok := defaults[t]; ok {
		return d
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
		if d, ok := defaults[t]; ok {
			return d
		}
	}
	if col.NeedsColumnType {
		return planrt.DefaultType{}
	}
	switch t {
	case reflect.TypeFor[bool]():
		return defaultInfo(tabula.TypeMapping(Bool()))
	case reflect.TypeFor[int16]():
		return defaultInfo(tabula.TypeMapping(tabula.CatalogType[int16]("pg_catalog", "int2", nil)))
	case reflect.TypeFor[int32]():
		return defaultInfo(tabula.TypeMapping(Int4()))
	case reflect.TypeFor[int64]():
		return defaultInfo(tabula.TypeMapping(Int8()))
	case reflect.TypeFor[float32]():
		return defaultInfo(tabula.TypeMapping(tabula.CatalogType[float32]("pg_catalog", "float4", nil)))
	case reflect.TypeFor[float64]():
		return defaultInfo(tabula.TypeMapping(tabula.CatalogType[float64]("pg_catalog", "float8", nil)))
	case reflect.TypeFor[string]():
		return defaultInfo(tabula.TypeMapping(Text()))
	case reflect.TypeFor[[]byte]():
		return defaultInfo(tabula.TypeMapping(tabula.CatalogType[[]byte]("pg_catalog", "bytea", nil)))
	case reflect.TypeFor[time.Time]():
		return defaultInfo(tabula.TypeMapping(Timestamptz()))
	default:
		return planrt.DefaultType{}
	}
}

func columnTypedRef[T any](col column[T], src *occurrence, kind catalogType) *fieldRef {
	return &fieldRef{source: src, name: col.Name, kind: kind, nullable: col.Nullable, secret: col.Secret}
}

// TableLayout holds the column types and scan targets of one registered table.
// Multiple aliases share the immutable layout and have distinct occurrences.
type TableLayout[T, S any] struct {
	owner *descriptor[T, S]
	types []planrt.DefaultType
	as    func(string) RowSource[T, S]
}

// MapTable declares a layout for a registered physical table. It panics on
// invalid declarations. Mapping or aliasing a table registers no statements.
func MapTable[T, K, S any](table *tabula.Table[T, K, S], overrides ...ColumnMapping) *TableLayout[T, S] {
	layout, err := TryMapTable(table, overrides...)
	if err != nil {
		panic(err)
	}
	return layout
}

// TryMapTable checks column overrides and resolves conventional types. Fields
// without a mapping can be omitted from a projection. A scope or soft-delete
// field used by the source must have a mapping.
func TryMapTable[T, K, S any](table *tabula.Table[T, K, S], overrides ...ColumnMapping) (*TableLayout[T, S], error) {
	tb := planTable(table)
	if tb == nil || tb.Entry == nil || tb.RelationScopeKind == 0 {
		return nil, fmt.Errorf("plan.MapTable: a registered physical table with a declared scope is required")
	}
	if _, err := parsePhysicalName(tb.Name); err != nil {
		return nil, err
	}
	declaredScope := relationScopeFromKind(tb.RelationScopeKind)
	types := make([]planrt.DefaultType, len(tb.Columns))
	for _, o := range overrides {
		i, ok := tb.ByName[o.name]
		if !ok || !typedIdent(o.name) {
			return nil, fmt.Errorf("plan.MapTable: unknown column %q", o.name)
		}
		if types[i].Kind.GoType != nil {
			return nil, fmt.Errorf("plan.MapTable: duplicate type for column %q", o.name)
		}
		field, value := tb.Columns[i].GoType, o.typ.Kind.GoType
		if !matchesColumnType(field, value) {
			return nil, fmt.Errorf("plan.MapTable: column %q holds %s, declared %v", o.name, field, value)
		}
		types[i] = o.typ
	}
	var special []int
	for i, col := range tb.Columns {
		if types[i].Kind.GoType == nil {
			types[i] = columnDefaultType(col, tb.DefaultTypes)
		}
		if i == tb.DeletedIdx || i == tb.ScopeIdx && declaredScope == tabula.ByScopeColumn() {
			if !typedIdent(col.Name) || types[i].Kind.GoType == nil {
				return nil, fmt.Errorf("plan.MapTable: declare a PostgreSQL type for column %q", col.Name)
			}
			special = append(special, i)
		}
	}
	var live rowLivePolicy
	if tb.DeletedIdx >= 0 {
		var err error
		live, err = deleteRowLivePolicy(tb.Delete, types[tb.DeletedIdx].Kind)
		if err != nil {
			return nil, err
		}
	}
	as := func(alias string) RowSource[T, S] {
		rel := FromTable(table, alias, declaredScope)
		columns := make([]RowColumn, len(special))
		for j, i := range special {
			columns[j] = RowColumn{ref: columnTypedRef(tb.Columns[i], rel.source, types[i].Kind)}
		}
		src := newRowSource(tb, rel, columns, &live)
		src.types = types
		return src
	}
	return &TableLayout[T, S]{owner: tb.Owner, types: types, as: as}, nil
}

// As creates a fresh occurrence using the table's declared scope and live-row
// policy. Reuse the returned source for fields, projections and joins.
func (layout *TableLayout[T, S]) As(alias string) RowSource[T, S] {
	if layout == nil || layout.as == nil {
		panic("plan.TableLayout.As: a declared layout is required")
	}
	return layout.as(alias)
}

// TableField identifies a typed column of one registered table across aliases.
type TableField[T, V any] struct {
	issuer *statementIssuer
	entry  *registration
	col    column[T]
	kind   tabula.PGType[V]
}

// MappedColumn resolves a layout field once. V must match its declared value
// type. A pointer field mapped through its element is referenced with that
// element type and retains its SQL nullability.
func MappedColumn[V, T, S any](layout *TableLayout[T, S], name string) TableField[T, V] {
	if layout == nil || layout.owner == nil {
		panic("plan.MappedColumn: a declared layout is required")
	}
	i, ok := layout.owner.ByName[name]
	if !ok || !typedIdent(name) {
		panic(fmt.Sprintf("plan.MappedColumn: unknown column %q", name))
	}
	d := layout.types[i]
	kind, ok := d.Typ.(tabula.PGType[V])
	if !ok {
		panic(fmt.Sprintf("plan.MappedColumn: column %q is mapped to %v, requested %s", name, d.Kind.GoType, reflect.TypeFor[V]()))
	}
	return TableField[T, V]{issuer: layout.owner.Issuer, entry: layout.owner.Entry,
		col: layout.owner.Columns[i], kind: kind}
}

// On selects this column on an occurrence of the same registered table.
func (f TableField[T, V]) On(rel Rel[T]) Field[V] {
	if rel.source == nil || rel.source.entry != f.entry || rel.source.issuer != f.issuer {
		panic("plan.TableField.On: the occurrence must belong to the mapped table")
	}
	return Field[V]{ref: columnTypedRef(f.col, rel.source, typeID(f.kind)), kind: f.kind}
}

// Input creates a distinct input with the field's declared type, codec and
// secret flag, including when its expression is used on another relation.
func (f TableField[T, V]) Input(name string) Slot[V] {
	slot := Input(name, f.kind)
	slot.input.secret = f.col.Secret
	return slot
}
