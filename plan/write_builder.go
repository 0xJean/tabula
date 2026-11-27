package plan

import (
	"fmt"
	"slices"
)

// InsertStatement declares a fixed INSERT without executing it. Values appends
// one row; DefaultValues and FromSelect select the other native input forms.
type InsertStatement struct{ node writeDeclaration }

// UpdateStatement declares an UPDATE with explicit assignments and matching.
type UpdateStatement struct{ node writeDeclaration }

// DeleteStatement declares a physical DELETE on a descriptor permitting it.
type DeleteStatement struct{ node writeDeclaration }

type writeDeclaration struct {
	target    rowSource
	columns   []*fieldRef
	command   string
	id        *statementEffectIdentity
	rows      [][]RowAssignment
	defaults  bool
	selection *ReadSelect
	insert    []RowColumn
	set       []RowAssignment
	where     []Pred
	all       bool
	version   RowVersion
	matches   []writeMatch
	conflict  conflictAction
	tables    []CommonTable
	returning []Projection
	unscoped  bool
	batch     []int
	aliases   []map[*typedInput]*typedInput
	err       error
}

type writeMatch struct {
	source ReadSource
	on     Pred
}

func newWriteDeclaration[T, S any](src RowSource[T, S], command string) writeDeclaration {
	n := writeDeclaration{target: src.rowSource, command: command, id: new(statementEffectIdentity)}
	if src.owner == nil || src.rel == nil {
		n.err = fmt.Errorf("tabula: a declared write target is required")
		return n
	}
	for i, col := range src.owner.Columns {
		n.columns = append(n.columns, columnTypedRef(col, src.rel, src.columnType(i)))
	}
	return n
}

// InsertInto starts a typed insertion into exactly this physical occurrence.
func InsertInto[T, S any](src RowSource[T, S]) InsertStatement {
	return InsertStatement{newWriteDeclaration(src, "INSERT")}
}

// UpdateOf starts a typed update. Where or AllRows is required.
func UpdateOf[T, S any](src RowSource[T, S]) UpdateStatement {
	return UpdateStatement{newWriteDeclaration(src, "UPDATE")}
}

// DeleteFrom starts a physical deletion. It requires AllowDelete and a fixed
// predicate or AllRows; the descriptor's live-row filter remains in force.
func DeleteFrom[T, S any](src RowSource[T, S]) DeleteStatement {
	return DeleteStatement{newWriteDeclaration(src, "DELETE")}
}

func (q InsertStatement) writeNode() statementNode { return q.node }
func (q UpdateStatement) writeNode() statementNode { return q.node }
func (q DeleteStatement) writeNode() statementNode { return q.node }

// Values appends one fixed VALUES row. Every row names the same ordered target
// columns. Repeated Slot identities within a row share one parameter.
func (q InsertStatement) Values(values ...RowAssignment) InsertStatement {
	q.node.rows = append(slices.Clip(q.node.rows), slices.Clone(values))
	return q
}

// DefaultValues requests PostgreSQL DEFAULT VALUES, including its native
// generated/default/identity behavior. Column-scoped insertion supplies scope.
func (q InsertStatement) DefaultValues() InsertStatement {
	q.node.defaults = true
	return q
}

// Columns names the ordered targets of FromSelect.
func (q InsertStatement) Columns(first RowColumn, rest ...RowColumn) InsertStatement {
	q.node.insert = slices.Concat([]RowColumn{first}, rest)
	return q
}

// FromSelect inserts the entire typed SELECT result. The declared SQL types,
// codecs, nullability and arity must agree with Columns before registration.
func (q InsertStatement) FromSelect(query ReadSelect) InsertStatement {
	q.node.selection = &query
	return q
}

// Batch declares the finite VALUES row counts accepted by ExecBatch/AllBatch.
// It requires one Values template and a size of 1 for every possible remainder.
// All chunks execute within one operation savepoint.
func (q InsertStatement) Batch(first int, rest ...int) InsertStatement {
	q.node.batch = slices.Concat([]int{first}, rest)
	return q
}

// Set declares the complete ordered UPDATE assignment list. Expressions read
// the old row. Protected keys, once, scope and version fields cannot be assigned.
func (q UpdateStatement) Set(first RowAssignment, rest ...RowAssignment) UpdateStatement {
	q.node.set = slices.Concat([]RowAssignment{first}, rest)
	return q
}

// Where appends fixed predicates to the UPDATE.
func (q UpdateStatement) Where(first Pred, rest ...Pred) UpdateStatement {
	q.node.where = slices.Concat(q.node.where, []Pred{first}, rest)
	return q
}

// Where appends fixed predicates to the DELETE.
func (q DeleteStatement) Where(first Pred, rest ...Pred) DeleteStatement {
	q.node.where = slices.Concat(q.node.where, []Pred{first}, rest)
	return q
}

// AllRows explicitly permits updating every authorized live row.
func (q UpdateStatement) AllRows() UpdateStatement { q.node.all = true; return q }

// AllRows explicitly permits deleting every authorized live row.
func (q DeleteStatement) AllRows() DeleteStatement { q.node.all = true; return q }

// Version adds the descriptor's optimistic version check and automatic increment.
func (q UpdateStatement) Version(version RowVersion) UpdateStatement {
	q.node.version = version
	return q
}

// Version adds the descriptor's optimistic version check to a physical DELETE.
func (q DeleteStatement) Version(version RowVersion) DeleteStatement {
	q.node.version = version
	return q
}

// From adds an explicitly matched UPDATE source. Value-bearing matches require
// an immediate non-null unique catalog key or a proven relational result key.
func (q UpdateStatement) From(source ReadSource, on Pred) UpdateStatement {
	q.node.matches = append(slices.Clip(q.node.matches), writeMatch{source, on})
	return q
}

// Using adds an explicitly matched DELETE source. Matching uniqueness is
// required when returned expressions depend on that source.
func (q DeleteStatement) Using(source ReadSource, on Pred) DeleteStatement {
	q.node.matches = append(slices.Clip(q.node.matches), writeMatch{source, on})
	return q
}

// UnscopedOnly declares an insertion usable only on an unscoped session. A
// column-scoped target then requires an explicit assignment to its scope column.
func (q InsertStatement) UnscopedOnly() InsertStatement { q.node.unscoped = true; return q }

// UnscopedOnly declares an update without column-scope predicates.
func (q UpdateStatement) UnscopedOnly() UpdateStatement { q.node.unscoped = true; return q }

// UnscopedOnly declares a deletion without column-scope predicates.
func (q DeleteStatement) UnscopedOnly() DeleteStatement { q.node.unscoped = true; return q }

// With attaches common tables to the outer INSERT.
func (q InsertStatement) With(tables ...CommonTable) InsertStatement {
	q.node.tables = slices.Concat(q.node.tables, tables)
	return q
}

// With attaches common tables to the outer UPDATE.
func (q UpdateStatement) With(tables ...CommonTable) UpdateStatement {
	q.node.tables = slices.Concat(q.node.tables, tables)
	return q
}

// With attaches common tables to the outer DELETE.
func (q DeleteStatement) With(tables ...CommonTable) DeleteStatement {
	q.node.tables = slices.Concat(q.node.tables, tables)
	return q
}

type defaultAssignment struct{}

func (defaultAssignment) render(*typedCompiler) (string, error) {
	return "", fmt.Errorf("tabula: DEFAULT is legal only as a write assignment")
}

// DefaultOf assigns PostgreSQL DEFAULT to this exact column. It is deliberately
// not an Expr and cannot escape into predicates or general SQL expressions.
func DefaultOf[V any](field Field[V]) RowAssignment {
	return RowAssignment{field: field.ref, part: defaultAssignment{}, valid: field.ref != nil}
}
