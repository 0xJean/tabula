package plan

import (
	"fmt"
	"reflect"

	"github.com/0xJean/tabula"
)

type occurrence struct {
	issuer      *statementIssuer
	entry       *registration
	table       physicalName
	alias       string
	scope       tabula.RelationScope
	scopeColumn string
	scopeSecret bool
	scopeGoType reflect.Type
	mode        tabula.WriteMode
	readOnly    bool
	allowDelete bool
	canUpdate   func(string) bool
}

// Rel identifies one occurrence of a physical table in a query. Two occurrences
// are distinct even when they use the same descriptor and alias.
type Rel[T any] struct {
	source *occurrence
	column func(string) (column[T], bool)
	rows   func() (rowSource, error)
}

// FromTable declares an occurrence of a registered, schema-qualified Table.
// The requested strategy must match the descriptor's declared enforcement.
func FromTable[T, K, S any](t *tabula.Table[T, K, S], alias string, scope tabula.RelationScope) Rel[T] {
	tb := planTable(t)
	if tb == nil || tb.Entry == nil || !typedIdent(alias) {
		panic("plan.FromTable: a registered table and valid alias are required")
	}
	declaredScope := relationScopeFromKind(tb.RelationScopeKind)
	if tb.RelationScopeKind == 0 || scope != declaredScope {
		panic("plan.FromTable: scope does not match the table declaration")
	}
	name, err := parsePhysicalName(tb.Name)
	if err != nil {
		panic(err)
	}
	src := &occurrence{
		issuer: tb.Issuer, entry: tb.Entry, table: name, alias: alias, scope: scope,
		mode: tabula.WriteMode(tb.WriteMode), readOnly: tb.ReadOnly, allowDelete: tb.AllowDelete, canUpdate: tb.AllowsUpdate,
	}
	if tb.ScopeIdx >= 0 {
		src.scopeColumn = tb.Columns[tb.ScopeIdx].Name
		src.scopeSecret = tb.Columns[tb.ScopeIdx].Secret
		src.scopeGoType = tb.Columns[tb.ScopeIdx].GoType
	}
	return Rel[T]{source: src, rows: func() (rowSource, error) { return tableRowSource(tb, src, nil, nil) }, column: func(name string) (column[T], bool) {
		i, ok := tb.ByName[name]
		if !ok {
			return column[T]{}, false
		}
		return tb.Columns[i], true
	}}
}

func (src *occurrence) checkCommand(command string) error {
	var allowed bool
	switch command {
	case "SELECT":
		allowed = src.mode == tabula.WriteDefault || src.mode == tabula.WriteUpdateOnly
	case "INSERT":
		allowed = src.mode == tabula.WriteDefault || src.mode == tabula.WriteInsertOnly
	case "UPDATE":
		allowed = src.mode == tabula.WriteDefault || src.mode == tabula.WriteUpdateOnly
	case "DELETE":
		allowed = src.mode == tabula.WriteDefault && src.allowDelete
	default:
		return fmt.Errorf("tabula: unsupported command %q for %s", command, src.table.SQL())
	}
	if command != "SELECT" && src.readOnly {
		return fmt.Errorf("%w: %s on %s", tabula.ErrReadOnly, command, src.table.SQL())
	}
	if !allowed {
		return fmt.Errorf("%w: %s on %s", tabula.ErrOperationDenied, command, src.table.SQL())
	}
	return nil
}

type fieldRef struct {
	source   *occurrence
	name     string
	kind     catalogType
	nullable bool
	secret   bool
}

// Field is a typed column on exactly one relation occurrence.
type Field[V any] struct {
	ref  *fieldRef
	kind tabula.PGType[V]
}

func matchesColumnType(field, value reflect.Type) bool {
	return field != nil && value != nil && (field == value || field.Kind() == reflect.Pointer && field.Elem() == value)
}

// FieldOf resolves a descriptor column at definition time. The Go value type
// must match. A pointer field may be read as its nullable element type.
func FieldOf[R, V any](r Rel[R], name string, kind tabula.PGType[V]) Field[V] {
	id := typeID(kind)
	if r.source == nil || r.column == nil || id.GoType == nil {
		panic("plan.FieldOf: an occurrence and a valid type are required")
	}
	c, ok := r.column(name)
	if !ok || !typedIdent(name) {
		panic(fmt.Sprintf("plan.FieldOf: unknown column %q", name))
	}
	if !matchesColumnType(c.GoType, id.GoType) {
		panic(fmt.Sprintf("plan.FieldOf: column %q holds %v, declared %v", name, c.GoType, id.GoType))
	}
	return Field[V]{ref: columnTypedRef(c, r.source, id), kind: kind}
}
