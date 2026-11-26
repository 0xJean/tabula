package tabula

import (
	"fmt"
	"slices"
)

// SortDirection selects the order of a composed query. SortDefault uses the
// primary term's fixed direction when set, then the QueryOrderSpec's default.
type SortDirection uint8

const (
	SortDefault SortDirection = iota
	SortAsc
	SortDesc
)

func (d SortDirection) sql() (string, error) {
	switch d {
	case SortAsc:
		return "ASC", nil
	case SortDesc:
		return "DESC", nil
	default:
		return "", fmt.Errorf("invalid sort direction %d", d)
	}
}

// QueryFilter declares one named, optional WHERE fragment of a composed query.
// Fragment parameters are numbered locally starting at $1 (independent of
// Select/Where base parameters) and may be reused across multiple predicates
// (for example "(o.code ILIKE $1 OR c.name ILIKE $1)").
type QueryFilter struct {
	Name   string
	SQL    string
	Secret bool
}

type scopeMode uint8

const (
	scopeUnset scopeMode = iota
	scopeColumns
	scopeByRLS
	scopeGlobal
)

// QueryScope declares how a composed query applies row scope on a scoped
// Registry. On a scoped Registry (S != struct{}), leaving Scope unset fails
// closed at DefineQuery time; choose ScopeColumns, ScopeByRLS or ScopeGlobal.
type QueryScope struct {
	mode scopeMode
	cols []string
}

// ScopeColumns injects outer WHERE predicates (<col> = $n) using the bound
// session's scope value on scoped sessions, omitting them on validated
// unscoped sessions. All listed columns share one parameter slot. Applies to
// driving and inner-joined tables; outer-joined tables must correlate in ON,
// use a composite foreign key or rely on PostgreSQL RLS.
func ScopeColumns(cols ...string) QueryScope {
	return QueryScope{mode: scopeColumns, cols: slices.Clone(cols)}
}

// ScopeByRLS declares that row scope is enforced by PostgreSQL RLS policies
// (or CTE/base predicates) rather than an outer scope column predicate.
func ScopeByRLS() QueryScope { return QueryScope{mode: scopeByRLS} }

// ScopeGlobal declares a view over global lookup tables that have no row scope.
func ScopeGlobal() QueryScope { return QueryScope{mode: scopeGlobal} }

// QuerySortTerm pairs a parameter-free SQL ordering expression with its matching View column.
type QuerySortTerm struct {
	Expr      string
	Column    string
	Direction SortDirection
}

// QuerySort declares one whitelisted caller sort key on QueryOrderSpec.
// Expr is a static SQL expression and cannot contain parameter placeholders.
type QuerySort struct {
	Key       string
	Expr      string
	Column    string
	Direction SortDirection
}

// QueryOrderSpec declares the whitelisted sort choices and optional tie-breaker
// terms of a composed query.
type QueryOrderSpec struct {
	Default          string
	DefaultDirection SortDirection
	Sorts            []QuerySort
	TieBreaker       []QuerySortTerm
}

// QuerySpec declares a view-owned composed query using explicit Select, Where,
// optional Tail (GROUP BY / HAVING) and optional optimized CountBase segments.
//
// Base parameters ($1..$k) share one namespace across Select, Where, Tail and
// CountBase. Filter fragments use fragment-local $1..$m numbering rebased at
// compile time.
type QuerySpec struct {
	Select    string
	Where     string
	Tail      string
	CountBase string
	Scope     QueryScope
	Filters   []QueryFilter
	Order     QueryOrderSpec
	MaxLimit  int
	// MaxOffset is the largest permitted QueryPage.Offset, inclusive. Zero allows any nonnegative offset.
	MaxOffset int
	Keyset    bool
}
