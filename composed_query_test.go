package tabula

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type cqRow struct {
	ID      int64  `db:"id"`
	Rank    int64  `db:"rank"`
	Due     bool   `db:"due"`
	Caption string `db:"caption"`
}

type cqNullableRow struct {
	ID       int64   `db:"id"`
	Optional *string `db:"optional"`
}

func TestComposedQueryCompileAndValidation(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{
		ScopeID: func(s customTenant) any { return string(s) },
	})
	view := DefineView(reg, ViewSpec[cqRow]{Name: "notes_view"})

	// Scoped registry rejects unset Scope.
	_, err := view.TryDefineQuery("bad_scope", QuerySpec{
		Select:   "SELECT n.id, n.rank, true AS due, '' AS caption FROM notes n",
		Order:    QueryOrderSpec{Default: "rank", Sorts: []QuerySort{{Key: "rank", Expr: "n.rank"}}},
		MaxLimit: 50,
	})
	require.ErrorContains(t, err, "explicit Scope")

	// Tail cannot combine with CountBase or Keyset.
	_, err = view.TryDefineQuery("bad_tail_count", QuerySpec{
		Select:    "SELECT n.id, n.rank, true AS due, '' AS caption FROM notes n",
		Tail:      "GROUP BY n.id, n.rank",
		CountBase: "SELECT count(*) FROM notes n",
		Scope:     ScopeByRLS(),
		Order:     QueryOrderSpec{Default: "rank", Sorts: []QuerySort{{Key: "rank", Expr: "n.rank"}}},
		MaxLimit:  50,
	})
	require.ErrorContains(t, err, "CountBase cannot combine with Tail")

	_, err = view.TryDefineQuery("bad_tail_keyset", QuerySpec{
		Select:   "SELECT n.id, n.rank, true AS due, '' AS caption FROM notes n",
		Tail:     "GROUP BY n.id, n.rank",
		Scope:    ScopeByRLS(),
		Keyset:   true,
		Order:    QueryOrderSpec{Default: "rank", Sorts: []QuerySort{{Key: "rank", Expr: "n.rank", Column: "rank"}}, TieBreaker: []QuerySortTerm{{Expr: "n.id", Column: "id"}}},
		MaxLimit: 50,
	})
	require.ErrorContains(t, err, "Keyset pagination cannot combine with Tail")

	// Nullable column rejected for Keyset.
	nullView := DefineView(reg, ViewSpec[cqNullableRow]{Name: "null_view"})
	_, err = nullView.TryDefineQuery("bad_null_ks", QuerySpec{
		Select:   "SELECT id, optional FROM notes",
		Scope:    ScopeGlobal(),
		Keyset:   true,
		Order:    QueryOrderSpec{Default: "opt", Sorts: []QuerySort{{Key: "opt", Expr: "optional", Column: "optional"}}, TieBreaker: []QuerySortTerm{{Expr: "id", Column: "id"}}},
		MaxLimit: 50,
	})
	require.ErrorContains(t, err, "nullable")

	// Mixed directions rejected in Keyset mode.
	_, err = view.TryDefineQuery("bad_mixed_ks", QuerySpec{
		Select: "SELECT id, rank, true AS due, '' AS caption FROM notes",
		Scope:  ScopeGlobal(),
		Keyset: true,
		Order: QueryOrderSpec{
			Default:    "rank",
			Sorts:      []QuerySort{{Key: "rank", Expr: "rank", Column: "rank"}},
			TieBreaker: []QuerySortTerm{{Expr: "id", Column: "id", Direction: SortAsc}},
		},
		MaxLimit: 50,
	})
	require.ErrorContains(t, err, "tie-breaker direction")

	// MaxLimit bounds check.
	_, err = view.TryDefineQuery("bad_limit", QuerySpec{
		Select:   "SELECT id, rank, true AS due, '' AS caption FROM notes",
		Scope:    ScopeGlobal(),
		Order:    QueryOrderSpec{Default: "rank", Sorts: []QuerySort{{Key: "rank", Expr: "rank"}}},
		MaxLimit: math.MaxInt,
	})
	require.ErrorContains(t, err, "MaxLimit")

	// Sparse base slots in CountBase + Where remap to dense $1..$m, and local filter $1 reuses argument cleanly.
	q := view.DefineQuery("list", QuerySpec{
		Select:    "SELECT n.id, n.rank, n.due_at <= $1::date AS due, coalesce(l.caption, '') AS caption FROM notes n LEFT JOIN labels l ON l.id = n.label_id AND $2::boolean",
		Where:     "n.visible OR n.pinned OR n.min_rank >= $3",
		CountBase: "SELECT count(*) FROM notes n",
		Scope:     ScopeColumns("n.scope_id"),
		Filters: []QueryFilter{
			{Name: "search", SQL: "n.title ILIKE $1 OR n.code ILIKE $1", Secret: true},
			{Name: "pinned_only", SQL: "n.pinned = true"},
			{Name: "range", SQL: "n.rank >= $1 AND n.rank <= $2"},
		},
		Order: QueryOrderSpec{
			Default:          "rank",
			DefaultDirection: SortDesc,
			Sorts: []QuerySort{
				{Key: "rank", Expr: "n.rank", Column: "rank"},
				{Key: "id", Expr: "n.id", Column: "id"},
			},
			TieBreaker: []QuerySortTerm{{Expr: "n.id", Column: "id"}},
		},
		MaxLimit: 100,
		Keyset:   true,
	})

	def := view.Definition()
	stmts := def.Statements
	for key := range stmts {
		require.Equal(t, "list", def.StatementQueries[key], key)
	}
	delete(def.StatementQueries, "list.scoped.page")
	require.Equal(t, "list", view.Definition().StatementQueries["list.scoped.page"])
	require.Contains(t, stmts, "list.scoped.page")
	require.Contains(t, stmts, "list.unscoped.page")
	require.Contains(t, stmts, "list.scoped.all.page")
	require.Contains(t, stmts, "list.scoped.all.count")
	require.Contains(t, stmts, "list.scoped.filter.search.page")
	require.Contains(t, stmts, "list.scoped.filter.search.count")
	require.Contains(t, stmts, "list.scoped.sort.id.page")
	require.Contains(t, stmts, "list.scoped.keyset.rank.first")
	require.Contains(t, stmts, "list.scoped.keyset.rank.after")

	// CountBase remapped base $3 -> $1, scope -> $2, search -> $3 (reused!).
	require.Equal(t,
		"SELECT count(*) FROM notes n\nWHERE (n.visible OR n.pinned OR n.min_rank >= $1)\n  AND (n.scope_id = $2)\n  AND (n.title ILIKE $3 OR n.code ILIKE $3)",
		stmts["list.scoped.filter.search.count"],
	)

	// Zero-value QueryParams works directly, defensive copy protects against caller mutation,
	// nil *QueryParams receiver panics on With/WithIf, and canonical declaration order is preserved.
	var emptyP QueryParams
	emptyP.With("pinned_only")
	require.Panics(t, func() { var nilP *QueryParams; nilP.With("pinned_only") })
	require.Panics(t, func() { var nilP *QueryParams; nilP.WithIf(false, "pinned_only") })

	baseSlice := []any{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), true, int64(1)}
	rangeSlice := []any{int64(2), int64(10)}
	p := NewQueryParams(baseSlice...).
		With("range", rangeSlice...).
		With("search", "%needle%") // reverse order from declaration!
	baseSlice[2] = int64(999)
	rangeSlice[0] = int64(999)

	var seenQueries []string
	var seenArgs [][]any
	sess := &queryArgRecorder{
		fakeSession: fakeSession{
			tenant: "t-7",
			scanFn: func(dest ...any) error { *dest[0].(*int64) = 2; return nil },
			eachRows: [][]any{
				{int64(2), int64(5), false, ""},
				{int64(1), int64(5), true, "blue"},
			},
		},
		onCall: func(sqlText string, args []any) {
			seenQueries = append(seenQueries, sqlText)
			seenArgs = append(seenArgs, args)
		},
	}

	got, total, err := q.Bind(sess).List(t.Context(), p, QueryPage{Size: 10, Offset: 0, Count: CountExact})
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	require.Len(t, got, 2)
	require.Len(t, seenQueries, 2)
	// Count received ONLY base slot $3 (int64(1), unmutated!), then scope, then search, then range (declaration order!).
	require.Equal(t, []any{int64(1), "t-7", "%needle%", int64(2), int64(10)}, seenArgs[0])
	// Page received all 3 base args, scope, search, range, limit, offset.
	require.Equal(t, []any{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), true, int64(1), "t-7", "%needle%", int64(2), int64(10), 10, 0}, seenArgs[1])

	// Deferred validation errors occur before I/O.
	seenQueries = nil
	_, _, err = q.Bind(sess).List(t.Context(), NewQueryParams(baseSlice...).With("unknown", 1), QueryPage{Size: 10})
	require.ErrorContains(t, err, "unknown filter")
	require.Empty(t, seenQueries)
	_, _, err = q.Bind(sess).List(t.Context(), NewQueryParams(baseSlice...).With("search", "a").With("search", "b"), QueryPage{Size: 10})
	require.ErrorContains(t, err, "duplicate filter")
	_, _, err = q.Bind(sess).List(t.Context(), NewQueryParams(baseSlice...), QueryPage{Size: 10, Count: CountEstimate})
	require.ErrorContains(t, err, "CountMode")
	_, err = q.Bind(sess).ListKeyset(t.Context(), p, QueryKeyset{Limit: 1, After: []any{"wrong-type", int64(1)}})
	require.ErrorContains(t, err, "expected int64")
}

func TestComposedSortExpressionParameters(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	view := DefineView(reg, ViewSpec[cqRow]{Name: "notes_view"})
	for _, tc := range []struct{ name, primary, tie, wantErr string }{
		{"primary_parameter", "rank + $1", "id", "Order.Sorts[\"rank\"]: sort Expr cannot contain parameter $1"},
		{"tie_parameter", "rank", "id + $2", "Order.TieBreaker[0]: sort Expr cannot contain parameter $2"},
		{"quoted_and_commented", "rank /* $1 */", "id + length('$2') - length($tag$$3$tag$)", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := view.TryDefineQuery(tc.name, QuerySpec{
				Select: "SELECT id, rank, true AS due, $1::text AS caption FROM notes",
				Scope:  ScopeGlobal(),
				Order: QueryOrderSpec{
					Default:    "rank",
					Sorts:      []QuerySort{{Key: "rank", Expr: tc.primary}},
					TieBreaker: []QuerySortTerm{{Expr: tc.tie}},
				},
				MaxLimit: 10,
			})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestComposedKeysetFixedDirectionSQL(t *testing.T) {
	const base = "SELECT id, rank, true AS due, '' AS caption FROM notes"
	for _, tc := range []struct {
		name           string
		fixed, request SortDirection
		wantSuffix     string
	}{
		{"ascending", SortAsc, SortDesc, "\nWHERE ((rank, id) > ($1, $2))\nORDER BY rank ASC, id ASC LIMIT $3"},
		{"descending", SortDesc, SortAsc, "\nWHERE ((rank, id) < ($1, $2))\nORDER BY rank DESC, id DESC LIMIT $3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := DefineView(NewRegistry(RegistryConfig[customTenant]{}), ViewSpec[cqRow]{Name: "notes_view"})
			query := view.DefineQuery("by_rank", QuerySpec{
				Select: base,
				Scope:  ScopeGlobal(),
				Order: QueryOrderSpec{
					Default:    "rank",
					Sorts:      []QuerySort{{Key: "rank", Expr: "rank", Column: "rank", Direction: tc.fixed}},
					TieBreaker: []QuerySortTerm{{Expr: "id", Column: "id", Direction: tc.fixed}},
				},
				Keyset: true, MaxLimit: 10,
			})
			var calls int
			session := &queryArgRecorder{onCall: func(statement string, args []any) {
				calls++
				require.Equal(t, base+tc.wantSuffix, statement)
				require.Equal(t, []any{int64(7), int64(2), 4}, args)
			}}
			rows := query.Bind(session)
			page := QueryKeyset{Direction: tc.request, Limit: 3, After: []any{int64(7), int64(2)}}
			_, err := rows.ListKeyset(t.Context(), nil, page)
			require.NoError(t, err)
			page.Direction = SortDirection(99)
			_, err = rows.ListKeyset(t.Context(), nil, page)
			require.ErrorContains(t, err, "invalid sort direction")
			require.Equal(t, 1, calls)
		})
	}
}

type queryArgRecorder struct {
	fakeSession
	onCall func(string, []any)
}

func (r *queryArgRecorder) Scan(ctx context.Context, q string, args []any, dest ...any) error {
	r.onCall(q, args)
	return r.fakeSession.Scan(ctx, q, args, dest...)
}

func (r *queryArgRecorder) Each(ctx context.Context, q string, args []any, dec func(func(...any) error) error) error {
	r.onCall(q, args)
	return r.fakeSession.Each(ctx, q, args, dec)
}
