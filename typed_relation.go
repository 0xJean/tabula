package tabula

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// RelationScope identifies the enforcement declared for a physical relation.
type RelationScope struct{ kind uint8 }

const (
	relationColumn uint8 = iota + 1
	relationRLS
	relationGlobal
)

// ByScopeColumn binds the session scope to the descriptor's tagged scope column.
func ByScopeColumn() RelationScope { return RelationScope{relationColumn} }

// ByRLS delegates scoping to PostgreSQL row-level security. A compiled shape
// records the physical relation for role-bound ENABLE and FORCE checks.
func ByRLS() RelationScope { return RelationScope{relationRLS} }

// Global declares a physical relation whose rows do not depend on session scope.
func Global() RelationScope { return RelationScope{relationGlobal} }

func (t *Table[T, K, S]) bindRelationScope(scope RelationScope) error {
	switch scope.kind {
	case 0:
		if t.scope >= 0 {
			scope = ByScopeColumn()
		}
	case relationColumn:
		if t.scope < 0 {
			return fmt.Errorf("ByScopeColumn requires a tagged scope column")
		}
	case relationGlobal:
		if t.scope >= 0 {
			return fmt.Errorf("Global conflicts with the tagged scope column")
		}
	case relationRLS:
		// A tagged scope remains available to legacy operations on this table.
	default:
		return fmt.Errorf("invalid relation scope")
	}
	t.relationScope = scope
	return nil
}

type physicalName struct{ schema, name string }

func parsePhysicalName(text string) (physicalName, error) {
	tokens, err := pgstmt.ScanFragment(text)
	if err != nil || len(tokens) != 3 || tokens[1].Text != "." {
		return physicalName{}, fmt.Errorf("tabula: typed tables need a schema-qualified catalog name")
	}
	n := physicalName{catalogIdentifier(tokens[0]), catalogIdentifier(tokens[2])}
	if !typedIdent(n.schema) || !typedIdent(n.name) {
		return physicalName{}, fmt.Errorf("tabula: invalid physical relation name")
	}
	return n, nil
}

func catalogIdentifier(token pgstmt.Span) string {
	switch token.Kind {
	case pgstmt.Word:
		return token.Text
	case pgstmt.Quoted:
		return strings.ReplaceAll(token.Text[1:len(token.Text)-1], `""`, `"`)
	default:
		return ""
	}
}

func (n physicalName) sql() string { return typedQuote(n.schema) + "." + typedQuote(n.name) }

type occurrence struct {
	issuer      *statementIssuer
	entry       *registration
	table       physicalName
	alias       string
	scope       RelationScope
	scopeColumn string
	scopeSecret bool
	scopeGoType reflect.Type
	mode        WriteMode
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
func FromTable[T, K, S any](t *Table[T, K, S], alias string, scope RelationScope) Rel[T] {
	if t == nil || t.entry == nil || !typedIdent(alias) {
		panic("tabula.FromTable: a registered table and valid alias are required")
	}
	if scope.kind == 0 || scope != t.relationScope {
		panic("tabula.FromTable: scope does not match the table declaration")
	}
	name, err := parsePhysicalName(t.name)
	if err != nil {
		panic(err)
	}
	src := &occurrence{
		issuer: t.reg.statementIssuer, entry: t.entry, table: name, alias: alias, scope: scope,
		mode: t.mode, readOnly: t.readOnly, allowDelete: t.allowDelete, canUpdate: t.allowsUpdateColumn,
	}
	if t.scope >= 0 {
		src.scopeColumn = t.cols[t.scope].name
		src.scopeSecret = t.cols[t.scope].secret
		src.scopeGoType = t.cols[t.scope].goType
	}
	return Rel[T]{source: src, rows: func() (rowSource, error) { return tableRowSource(t, src, nil) }, column: func(name string) (column[T], bool) {
		i, ok := t.byName[name]
		if !ok {
			return column[T]{}, false
		}
		return t.cols[i], true
	}}
}

func (src *occurrence) checkCommand(command string) error {
	var op tableOperation
	switch command {
	case "SELECT":
		op = selectOp
	case "INSERT":
		op = insertOp
	case "UPDATE":
		op = updateOp
	case "DELETE":
		op = deleteOp
	default:
		return fmt.Errorf("tabula: unsupported command %q for %s", command, src.table.sql())
	}
	if op != selectOp && src.readOnly {
		return fmt.Errorf("%w: %s on %s", ErrReadOnly, command, src.table.sql())
	}
	if !src.mode.allows(op) || op == deleteOp && !src.allowDelete {
		return fmt.Errorf("%w: %s on %s", ErrOperationDenied, command, src.table.sql())
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
	kind PGType[V]
}

// FieldOf resolves a descriptor column at definition time. The Go value type
// must match. A pointer field may be read as its nullable element type.
func FieldOf[R, V any](r Rel[R], name string, kind PGType[V]) Field[V] {
	if r.source == nil || r.column == nil || kind.id.goType == nil {
		panic("tabula.FieldOf: an occurrence and a valid type are required")
	}
	c, ok := r.column(name)
	if !ok || !typedIdent(name) {
		panic(fmt.Sprintf("tabula.FieldOf: unknown column %q", name))
	}
	match := c.goType == kind.id.goType || (c.goType.Kind() == reflect.Pointer && c.goType.Elem() == kind.id.goType)
	if !match {
		panic(fmt.Sprintf("tabula.FieldOf: column %q holds %v, declared %v", name, c.goType, kind.id.goType))
	}
	return Field[V]{&fieldRef{r.source, name, kind.id, c.nullable, c.secret}, kind}
}
