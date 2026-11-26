package tabula

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
)

// CountMode controls how Rows.List obtains the total row count.
type CountMode int

const (
	// CountExact runs SELECT count(*) with the active filter (default).
	CountExact CountMode = iota
	// CountNone skips the count query and returns -1 for total.
	CountNone
	// CountEstimate reads PostgreSQL's pg_class.reltuples estimate for the
	// table instead of scanning rows with COUNT(*).
	CountEstimate
)

// KeysetListing requests one cursor-paginated page ordered by
// (SortColumn, key columns) without OFFSET scan degradation.
type KeysetListing struct {
	// SortColumn optionally specifies a primary sort column (must be declared
	// on the Table). When empty, pagination orders directly by the key columns.
	SortColumn string
	// Direction is "ASC" (default when empty) or "DESC".
	Direction string
	// After holds the cursor values from the last item of the previous page,
	// matching the keyset tuple order: (SortColumn, key...) or (key...) when
	// SortColumn is empty or already part of the key.
	After []any
	// Limit is the maximum number of items to return (must be > 0). The engine
	// fetches Limit+1 rows to determine HasMore accurately.
	Limit int
	// Lock selects the row-lock clause and remembers locked rows for once checks.
	Lock LockMode
}

// KeysetPage is one cursor-paginated slice plus whether another page exists.
// When HasMore is true, Next carries the cursor values extracted from the last
// returned item in tuple order for the subsequent After call.
type KeysetPage[T any] struct {
	Items   []T
	HasMore bool
	Next    []any
}

// ListKeyset reads one cursor-paginated page of undeleted rows matching where
// in deterministic (SortColumn, key...) order without OFFSET. Invalid listing
// options return an error before the filter callback or database is used.
func (r *Rows[T, K, S]) ListKeyset(ctx context.Context, where func(*Filter), l KeysetListing) (KeysetPage[T], error) {
	if err := r.checkOperation(selectOp); err != nil {
		return KeysetPage[T]{}, err
	}
	if l.Limit <= 0 || l.Limit == math.MaxInt {
		return KeysetPage[T]{}, fmt.Errorf("%s.ListKeyset %s: Limit must be positive and less than math.MaxInt", r.t.prefix(), r.t.name)
	}
	if l.Lock > LockShare {
		return KeysetPage[T]{}, fmt.Errorf("%s.ListKeyset %s: invalid lock mode %d", r.t.prefix(), r.t.name, l.Lock)
	}
	dir := strings.ToUpper(strings.TrimSpace(l.Direction))
	if dir == "" {
		dir = "ASC"
	}
	if dir != "ASC" && dir != "DESC" {
		return KeysetPage[T]{}, fmt.Errorf("%s.ListKeyset %s: invalid direction %q", r.t.prefix(), r.t.name, l.Direction)
	}

	tupleIdx := make([]int, 0, len(r.t.keys)+1)
	if l.SortColumn != "" {
		idx, ok := r.t.byName[l.SortColumn]
		if !ok {
			return KeysetPage[T]{}, fmt.Errorf("%s.ListKeyset %s: sort column not declared: %s", r.t.prefix(), r.t.name, l.SortColumn)
		}
		tupleIdx = append(tupleIdx, idx)
	}
	for _, keyIdx := range r.t.keys {
		if !slices.Contains(tupleIdx, keyIdx) {
			tupleIdx = append(tupleIdx, keyIdx)
		}
	}

	if len(l.After) > 0 && len(l.After) != len(tupleIdx) {
		return KeysetPage[T]{}, fmt.Errorf("%s.ListKeyset %s: After cursor has %d values, expected %d", r.t.prefix(), r.t.name, len(l.After), len(tupleIdx))
	}
	if l.Lock != LockNone && r.readOnly() {
		return KeysetPage[T]{}, ErrReadOnly
	}
	f := r.Scope()
	if where != nil {
		where(f)
	}
	if len(l.After) > 0 {
		op := ">"
		if dir == "DESC" {
			op = "<"
		}
		if len(tupleIdx) == 1 {
			f.And(r.name(tupleIdx[0])+" "+op+" ?", l.After[0])
		} else {
			cols := make([]string, len(tupleIdx))
			holders := make([]string, len(tupleIdx))
			for i, idx := range tupleIdx {
				cols[i] = r.name(idx)
				holders[i] = "?"
			}
			f.And("("+strings.Join(cols, ", ")+") "+op+" ("+strings.Join(holders, ", ")+")", l.After...)
		}
	}

	orderParts := make([]string, len(tupleIdx))
	for i, idx := range tupleIdx {
		orderParts[i] = r.name(idx) + " " + dir
	}
	query := r.t.stmt.list + f.Where() + " ORDER BY " + strings.Join(orderParts, ", ") + " LIMIT " + f.Arg(l.Limit+1)
	query += l.Lock.clause()
	rows, err := r.selectOp(ctx, "list_keyset", stmtMeta{key: r.t.name + ".list_keyset"}, query, f.Args())
	if err != nil {
		return KeysetPage[T]{}, err
	}
	hasMore := false
	var nextCursor []any
	if len(rows) > l.Limit {
		hasMore = true
		rows = rows[:l.Limit]
		last := rows[len(rows)-1]
		nextCursor = make([]any, len(tupleIdx))
		for i, idx := range tupleIdx {
			nextCursor[i] = reflect.ValueOf(r.t.cols[idx].field(&last)).Elem().Interface()
		}
	}
	if l.Lock != LockNone {
		for _, row := range rows {
			r.remember(row)
		}
	}
	return KeysetPage[T]{Items: rows, HasMore: hasMore, Next: nextCursor}, nil
}

// LockMode selects the PostgreSQL row-lock clause of a listing.
type LockMode uint8

const (
	LockNone       LockMode = iota
	LockUpdate              // FOR UPDATE
	LockSkipLocked          // FOR UPDATE SKIP LOCKED
	LockNoWait              // FOR UPDATE NOWAIT
	LockShare               // FOR SHARE
)

func (m LockMode) clause() string {
	switch m {
	case LockNone:
		return ""
	case LockUpdate:
		return " FOR UPDATE"
	case LockSkipLocked:
		return " FOR UPDATE SKIP LOCKED"
	case LockNoWait:
		return " FOR UPDATE NOWAIT"
	case LockShare:
		return " FOR SHARE"
	default:
		panic(fmt.Sprintf("tabula: invalid lock mode %d", m))
	}
}
