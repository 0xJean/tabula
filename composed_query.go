package tabula

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"reflect"
	"slices"
)

type paramEntry struct {
	name string
	args []any
}

// QueryParams holds base arguments and named filter arguments for a composed
// Query. The zero value is directly usable (like strings.Builder and Filter)
// for queries without base arguments:
//
//	var p tabula.QueryParams
//	if since != nil {
//	    p.With("since", *since)
//	}
//	rows, total, err := q.Bind(sess).List(ctx, &p, page)
//
// NewQueryParams and With/WithIf defensively copy their argument slices. Errors
// such as unknown or duplicate filter names and arity mismatches are reported
// by the bound query operation before any I/O. Once built, a *QueryParams is
// read-only during execution and safe to reuse across operations (such as Count
// followed by List), though it is not safe for concurrent mutation.
type QueryParams struct {
	base    []any
	entries []paramEntry
}

// NewQueryParams creates a *QueryParams seeded with baseArgs ($1..$k).
func NewQueryParams(baseArgs ...any) *QueryParams {
	return &QueryParams{base: slices.Clone(baseArgs)}
}

// With enables the named filter with args (0, 1 or more values matching the
// filter's fragment-local $1..$m placeholders) and returns p for chaining.
// Calling With on a nil *QueryParams panics so conditional code never silently
// drops a filter. A supplied nil (including a typed nil) binds SQL NULL.
func (p *QueryParams) With(name string, args ...any) *QueryParams {
	if p == nil {
		panic("tabula.QueryParams.With: nil *QueryParams receiver")
	}
	p.entries = append(p.entries, paramEntry{name: name, args: slices.Clone(args)})
	return p
}

// WithIf calls With(name, args...) when cond is true and returns p unchanged
// otherwise. Calling WithIf on a nil *QueryParams panics.
//
// Caution: Go evaluates function arguments before entering WithIf. Never write
// `p.WithIf(since != nil, "since", *since)` — dereferencing `*since` panics
// when `since` is nil. Guard pointer dereferences with an ordinary `if` block:
//
//	if since != nil {
//	    p.With("since", *since)
//	}
func (p *QueryParams) WithIf(cond bool, name string, args ...any) *QueryParams {
	if p == nil {
		panic("tabula.QueryParams.WithIf: nil *QueryParams receiver")
	}
	if !cond {
		return p
	}
	return p.With(name, args...)
}

// QueryPage requests an offset-paginated result from QueryRows.List.
// Direction applies to sort terms declared with SortDefault. Fixed terms keep their direction.
type QueryPage struct {
	Sort      string
	Direction SortDirection
	Size      int
	Offset    int
	Count     CountMode
}

// QueryKeyset requests a cursor-paginated result from QueryRows.ListKeyset.
// A fixed sort direction governs both the row order and the After comparison.
// Direction selects the order for sorts declared with SortDefault.
type QueryKeyset struct {
	Sort      string
	Direction SortDirection
	Limit     int
	After     []any
}

// QueryOrder selects sorting for unpaged QueryRows.Find and QueryRows.Iter.
// Direction applies to sort terms declared with SortDefault. Fixed terms keep their direction.
type QueryOrder struct {
	Sort      string
	Direction SortDirection
}

// QueryRows is a compiled Query bound to one Session.
type QueryRows[T, S any] struct {
	readBinding[T, S]
	q *Query[T, S]
}

// Bind binds q to session s, applying the owning Registry's A4 SessionKey check.
func (q *Query[T, S]) Bind(s Session[S]) *QueryRows[T, S] {
	return &QueryRows[T, S]{
		readBinding: bindReader(&q.v.descriptor, s),
		q:           q,
	}
}

// WithLogger returns a handle with a per-binding logger override, retaining any
// A4 rejection error.
func (r *QueryRows[T, S]) WithLogger(logger *slog.Logger) *QueryRows[T, S] {
	clone := *r
	clone.logger = logger
	return &clone
}

type boundQueryArgs struct {
	scoped      bool
	enabled     []int
	pageArgs    []any
	pageSecret  []bool
	countArgs   []any
	countSecret []bool
}

func (r *QueryRows[T, S]) resolveParams(p *QueryParams) (boundQueryArgs, error) {
	var base []any
	var entries []paramEntry
	if p != nil {
		base = p.base
		entries = p.entries
	}
	if len(base) != r.q.baseArity {
		return boundQueryArgs{}, fmt.Errorf("%s.Query %s.%s: got %d base args, expected %d", r.q.v.prefix(), r.q.v.name, r.q.key, len(base), r.q.baseArity)
	}
	byIdx := make(map[int][]any, len(entries))
	for _, e := range entries {
		idx, ok := r.q.filterIndex[e.name]
		if !ok {
			return boundQueryArgs{}, fmt.Errorf("%s.Query %s.%s: unknown filter %q", r.q.v.prefix(), r.q.v.name, r.q.key, e.name)
		}
		if _, dup := byIdx[idx]; dup {
			return boundQueryArgs{}, fmt.Errorf("%s.Query %s.%s: duplicate filter %q", r.q.v.prefix(), r.q.v.name, r.q.key, e.name)
		}
		want := r.q.filters[idx].frag.arity
		if len(e.args) != want {
			return boundQueryArgs{}, fmt.Errorf("%s.Query %s.%s: filter %q expects %d args, got %d", r.q.v.prefix(), r.q.v.name, r.q.key, e.name, want, len(e.args))
		}
		byIdx[idx] = e.args
	}
	scoped := r.q.scopeMode == scopeColumns && !r.s.Unscoped()

	// Canonical declaration order!
	enabled := make([]int, 0, len(byIdx))
	for i := range r.q.filters {
		if _, ok := byIdx[i]; ok {
			enabled = append(enabled, i)
		}
	}

	pageArgs := make([]any, 0, len(base)+1+len(entries)*2)
	pageSecret := make([]bool, 0, cap(pageArgs))
	pageArgs = append(pageArgs, base...)
	for range base {
		pageSecret = append(pageSecret, false)
	}

	countArgs := make([]any, 0, len(r.q.countSlots)+1+len(entries)*2)
	countSecret := make([]bool, 0, cap(countArgs))
	for _, slot := range r.q.countSlots {
		countArgs = append(countArgs, base[slot-1])
		countSecret = append(countSecret, false)
	}

	if scoped {
		sv := r.q.v.reg.scopeValue(r.s.Scope())
		pageArgs = append(pageArgs, sv)
		pageSecret = append(pageSecret, false)
		countArgs = append(countArgs, sv)
		countSecret = append(countSecret, false)
	}

	for _, idx := range enabled {
		flt := r.q.filters[idx]
		vals := byIdx[idx]
		pageArgs = append(pageArgs, vals...)
		countArgs = append(countArgs, vals...)
		for range vals {
			pageSecret = append(pageSecret, flt.secret)
			countSecret = append(countSecret, flt.secret)
		}
	}

	return boundQueryArgs{
		scoped:      scoped,
		enabled:     enabled,
		pageArgs:    pageArgs,
		pageSecret:  pageSecret,
		countArgs:   countArgs,
		countSecret: countSecret,
	}, nil
}

// List reads one offset-paginated page plus optional exact total count.
// CountMode must be CountExact (default) or CountNone (total = -1).
func (r *QueryRows[T, S]) List(ctx context.Context, params *QueryParams, page QueryPage) ([]T, int64, error) {
	if r.bindErr != nil {
		return nil, 0, r.bindErr
	}
	if page.Size < 1 || page.Size > r.q.maxLimit || page.Offset < 0 {
		return nil, 0, fmt.Errorf("%s.Query %s.%s: invalid page size=%d offset=%d (max=%d)", r.q.v.prefix(), r.q.v.name, r.q.key, page.Size, page.Offset, r.q.maxLimit)
	}
	if page.Count != CountExact && page.Count != CountNone {
		return nil, 0, fmt.Errorf("%s.Query %s.%s: unsupported CountMode %d (use CountExact or CountNone)", r.q.v.prefix(), r.q.v.name, r.q.key, page.Count)
	}
	bound, err := r.resolveParams(params)
	if err != nil {
		return nil, 0, err
	}
	pageSQL, err := r.q.buildSelectSQL(bound.scoped, bound.enabled, page.Sort, page.Direction, true, false, false)
	if err != nil {
		return nil, 0, err
	}
	total := int64(-1)
	if page.Count == CountExact {
		countSQL, err := r.q.buildCountSQL(bound.scoped, bound.enabled)
		if err != nil {
			return nil, 0, err
		}
		meta := r.q.meta
		meta.key = r.q.v.name + "." + r.q.key + ".count"
		if err := r.scanSecret(ctx, "count", meta, countSQL, bound.countArgs, bound.countSecret, &total); err != nil {
			return nil, 0, err
		}
	}
	pArgs := append(bound.pageArgs, page.Size, page.Offset)
	pSecret := append(bound.pageSecret, false, false)
	meta := r.q.meta
	meta.key = r.q.v.name + "." + r.q.key + ".list"
	rows, err := collect(r.streamSecret(ctx, "list", meta, pageSQL, pArgs, pSecret))
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// Count returns the exact number of matching rows before pagination.
func (r *QueryRows[T, S]) Count(ctx context.Context, params *QueryParams) (int64, error) {
	if r.bindErr != nil {
		return 0, r.bindErr
	}
	bound, err := r.resolveParams(params)
	if err != nil {
		return 0, err
	}
	countSQL, err := r.q.buildCountSQL(bound.scoped, bound.enabled)
	if err != nil {
		return 0, err
	}
	var total int64
	meta := r.q.meta
	meta.key = r.q.v.name + "." + r.q.key + ".count"
	if err := r.scanSecret(ctx, "count", meta, countSQL, bound.countArgs, bound.countSecret, &total); err != nil {
		return 0, err
	}
	return total, nil
}

// Find reads all matching rows in the selected order without running COUNT(*).
func (r *QueryRows[T, S]) Find(ctx context.Context, params *QueryParams, order QueryOrder) ([]T, error) {
	if r.bindErr != nil {
		return nil, r.bindErr
	}
	bound, err := r.resolveParams(params)
	if err != nil {
		return nil, err
	}
	sqlText, err := r.q.buildSelectSQL(bound.scoped, bound.enabled, order.Sort, order.Direction, false, false, false)
	if err != nil {
		return nil, err
	}
	meta := r.q.meta
	meta.key = r.q.v.name + "." + r.q.key + ".find"
	return collect(r.streamSecret(ctx, "find", meta, sqlText, bound.pageArgs, bound.pageSecret))
}

// Iter streams all matching rows in the selected order. Breaking the range
// closes the cursor.
func (r *QueryRows[T, S]) Iter(ctx context.Context, params *QueryParams, order QueryOrder) iter.Seq2[T, error] {
	if r.bindErr != nil {
		return failedIter[T](r.bindErr)
	}
	bound, err := r.resolveParams(params)
	if err != nil {
		return failedIter[T](err)
	}
	sqlText, err := r.q.buildSelectSQL(bound.scoped, bound.enabled, order.Sort, order.Direction, false, false, false)
	if err != nil {
		return failedIter[T](err)
	}
	meta := r.q.meta
	meta.key = r.q.v.name + "." + r.q.key + ".iter"
	return r.streamSecret(ctx, "iter", meta, sqlText, bound.pageArgs, bound.pageSecret)
}

// ListKeyset reads one cursor-paginated page without OFFSET, fetching Limit+1
// rows to set HasMore and populating Next from the last returned item.
func (r *QueryRows[T, S]) ListKeyset(ctx context.Context, params *QueryParams, page QueryKeyset) (KeysetPage[T], error) {
	if r.bindErr != nil {
		return KeysetPage[T]{}, r.bindErr
	}
	if !r.q.keyset {
		return KeysetPage[T]{}, fmt.Errorf("%s.Query %s.%s: keyset pagination is not enabled on this query", r.q.v.prefix(), r.q.v.name, r.q.key)
	}
	if page.Limit < 1 || page.Limit > r.q.maxLimit {
		return KeysetPage[T]{}, fmt.Errorf("%s.Query %s.%s: invalid keyset limit=%d (max=%d)", r.q.v.prefix(), r.q.v.name, r.q.key, page.Limit, r.q.maxLimit)
	}
	cs, _, _, err := r.q.resolveOrder(page.Sort, page.Direction)
	if err != nil {
		return KeysetPage[T]{}, err
	}
	if len(page.After) > 0 {
		if len(page.After) != len(cs.terms) {
			return KeysetPage[T]{}, fmt.Errorf("%s.Query %s.%s: After cursor has %d values, expected %d", r.q.v.prefix(), r.q.v.name, r.q.key, len(page.After), len(cs.terms))
		}
		for i, val := range page.After {
			if val == nil {
				return KeysetPage[T]{}, fmt.Errorf("%s.Query %s.%s: After[%d] must not be nil", r.q.v.prefix(), r.q.v.name, r.q.key, i)
			}
			wantType := r.q.v.cols[cs.terms[i].colIdx].goType
			gotType := reflect.TypeOf(val)
			if gotType != wantType {
				return KeysetPage[T]{}, fmt.Errorf("%s.Query %s.%s: After[%d] has type %s, expected %s", r.q.v.prefix(), r.q.v.name, r.q.key, i, gotType, wantType)
			}
		}
	}
	bound, err := r.resolveParams(params)
	if err != nil {
		return KeysetPage[T]{}, err
	}
	sqlText, err := r.q.buildSelectSQL(bound.scoped, bound.enabled, page.Sort, page.Direction, false, true, len(page.After) > 0)
	if err != nil {
		return KeysetPage[T]{}, err
	}
	args := bound.pageArgs
	secret := bound.pageSecret
	if len(page.After) > 0 {
		args = append(args, page.After...)
		for range page.After {
			secret = append(secret, false)
		}
	}
	args = append(args, page.Limit+1)
	secret = append(secret, false)
	meta := r.q.meta
	meta.key = r.q.v.name + "." + r.q.key + ".list_keyset"
	rows, err := collect(r.streamSecret(ctx, "list_keyset", meta, sqlText, args, secret))
	if err != nil {
		return KeysetPage[T]{}, err
	}
	hasMore := len(rows) > page.Limit
	var next []any
	if hasMore {
		rows = rows[:page.Limit]
		last := rows[len(rows)-1]
		next = make([]any, len(cs.terms))
		for i, term := range cs.terms {
			next[i] = reflect.ValueOf(r.q.v.cols[term.colIdx].field(&last)).Elem().Interface()
		}
	}
	return KeysetPage[T]{Items: rows, HasMore: hasMore, Next: next}, nil
}

func (r *readBinding[T, S]) scanSecret(ctx context.Context, action string, meta stmtMeta, query string, args []any, secret []bool, dest ...any) error {
	if r.bindErr != nil {
		return r.bindErr
	}
	ctx, span := r.start(ctx, action, meta, secret)
	err := r.s.Scan(ctx, query, args, dest...)
	span.finish(ctx, query, -1, err, args, secret, meta.secret, meta.quiet)
	return r.fail(ctx, err)
}

func (r *readBinding[T, S]) streamSecret(ctx context.Context, action string, meta stmtMeta, query string, args []any, secret []bool) iter.Seq2[T, error] {
	if r.bindErr != nil {
		return failedIter[T](r.bindErr)
	}
	return func(yield func(T, error) bool) {
		opCtx, span := r.start(ctx, action, meta, secret)
		var count int64
		dst := make([]any, len(r.d.cols))
		stopped := false
		err := r.s.Each(opCtx, query, args, func(scan func(...any) error) error {
			var row T
			if err := scan(r.fillAddrs(dst, &row)...); err != nil {
				return err
			}
			count++
			if !yield(row, nil) {
				stopped = true
				return stopIteration
			}
			return nil
		})
		if errors.Is(err, stopIteration) {
			err = nil
		}
		span.finish(opCtx, query, count, err, args, secret, meta.secret, meta.quiet)
		if err != nil && !stopped {
			var zero T
			yield(zero, r.fail(opCtx, err))
		}
	}
}
