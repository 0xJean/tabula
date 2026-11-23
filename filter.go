package tabula

import (
	"strconv"
	"strings"
)

// Filter accumulates optional predicates of one query over numbered bind
// parameters ($1..$n). Every value is bound; clause text is static adapter SQL.
//
// And replaces each single "?" of its clause, in order, with the next
// parameter. Write "??" to emit a literal "?" (for PostgreSQL JSONB operators
// such as ?, ?|, ?&). A marker/value mismatch is not repaired so malformed
// clauses fail loudly at prepare or execution.
type Filter struct {
	args  []any
	where []string
}

// NewFilter starts a filter whose fixed values occupy $1..$n.
func NewFilter(fixed ...any) *Filter {
	return &Filter{args: append([]any(nil), fixed...)}
}

// Arg binds one value and returns its placeholder.
func (f *Filter) Arg(v any) string {
	f.args = append(f.args, v)
	return "$" + strconv.Itoa(len(f.args))
}

// And appends one predicate; it is parenthesized when joined.
func (f *Filter) And(clause string, values ...any) {
	var b strings.Builder
	b.Grow(len(clause) + len(values)*2)
	valIdx := 0
	for i := 0; i < len(clause); i++ {
		if clause[i] != '?' {
			b.WriteByte(clause[i])
			continue
		}
		if i+1 < len(clause) && clause[i+1] == '?' {
			b.WriteByte('?')
			i++
			continue
		}
		if valIdx < len(values) {
			b.WriteString(f.Arg(values[valIdx]))
			valIdx++
		} else {
			b.WriteByte('?')
		}
	}
	for ; valIdx < len(values); valIdx++ {
		f.Arg(values[valIdx])
	}
	f.where = append(f.where, b.String())
}

// Where returns the predicates as " AND (p1) AND (p2)", or "" when none.
func (f *Filter) Where() string {
	if len(f.where) == 0 {
		return ""
	}
	return " AND (" + strings.Join(f.where, ") AND (") + ")"
}

// Joined returns the predicates joined by " AND " without leading connector.
func (f *Filter) Joined() string { return strings.Join(f.where, " AND ") }

// Page binds limit and offset and returns " LIMIT $a OFFSET $b".
func (f *Filter) Page(limit, offset any) string {
	l := f.Arg(limit)
	return " LIMIT " + l + " OFFSET " + f.Arg(offset)
}

// Args returns a copy of the bound values in placeholder order.
func (f *Filter) Args() []any { return append([]any(nil), f.args...) }
