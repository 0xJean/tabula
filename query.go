package tabula

import (
	"context"
	"fmt"
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
	// Lock appends FOR UPDATE to the query and remembers once columns.
	Lock bool
}

// KeysetPage is one cursor-paginated slice plus whether another page exists.
type KeysetPage[T any] struct {
	Items   []T
	HasMore bool
}

// ListKeyset reads one cursor-paginated page of undeleted rows matching where
// in deterministic (SortColumn, key...) order without OFFSET.
func (r *Rows[T, K, S]) ListKeyset(ctx context.Context, tenant S, where func(*Filter), l KeysetListing) (KeysetPage[T], error) {
	if l.Limit <= 0 {
		panic(r.t.prefix() + ".ListKeyset " + r.t.Name + ": Limit must be positive")
	}
	dir := strings.ToUpper(strings.TrimSpace(l.Direction))
	if dir == "" {
		dir = "ASC"
	}
	if dir != "ASC" && dir != "DESC" {
		panic(fmt.Sprintf("%s.ListKeyset %s: invalid direction %q", r.t.prefix(), r.t.Name, l.Direction))
	}

	tupleIdx := make([]int, 0, len(r.t.keys)+1)
	if l.SortColumn != "" {
		idx, ok := r.t.byName[l.SortColumn]
		if !ok {
			panic(r.t.prefix() + ".ListKeyset " + r.t.Name + ": sort column not declared: " + l.SortColumn)
		}
		tupleIdx = append(tupleIdx, idx)
	}
	for _, keyIdx := range r.t.keys {
		if !slices.Contains(tupleIdx, keyIdx) {
			tupleIdx = append(tupleIdx, keyIdx)
		}
	}

	f := r.Scope()
	if where != nil {
		where(f)
	}
	if len(l.After) > 0 {
		if len(l.After) != len(tupleIdx) {
			panic(fmt.Sprintf("%s.ListKeyset %s: After cursor has %d values, expected %d", r.t.prefix(), r.t.Name, len(l.After), len(tupleIdx)))
		}
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
	if l.Lock {
		query += " FOR UPDATE"
	}
	rows, err := r.selectOp(ctx, tenant, "list_keyset", r.t.Name+".list_keyset", query, f.Args(), false, false)
	if err != nil {
		return KeysetPage[T]{}, err
	}
	hasMore := false
	if len(rows) > l.Limit {
		hasMore = true
		rows = rows[:l.Limit]
	}
	if l.Lock {
		for _, row := range rows {
			r.remember(row)
		}
	}
	return KeysetPage[T]{Items: rows, HasMore: hasMore}, nil
}
