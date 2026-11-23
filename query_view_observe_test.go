package tabula

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFilterHelpersAndIfInRange(t *testing.T) {
	f := NewFilter("t1")
	f.AndIf(false, "ignored = ?", "x")
	f.AndIf(true, "status = ?", "active")
	f.In("code", "alpha", "beta")
	var nilTime *time.Time
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.Range("created_at", start, nilTime)
	require.Equal(t, " AND (status = $2) AND (code IN ($3, $4)) AND (created_at >= $5)", f.Where())
	require.Equal(t, []any{"t1", "active", "alpha", "beta", start}, f.Args())

	empty := NewFilter()
	empty.In("code")
	require.Equal(t, " AND (FALSE)", empty.Where())
	require.Empty(t, empty.Args())
}

func TestCountModesAndKeysetPagination(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{
		ScopeID: func(s customTenant) any { return string(s) },
	})
	tbl := Define(reg, TableSpec[customRow, int64]{
		Name:  "public.custom_items",
		Order: "id ASC",
	})

	var queries []string
	sess := &queryRecorderSession{
		fakeSession: fakeSession{
			tenant: "t-1",
			scanFn: func(dest ...any) error {
				*dest[0].(*int64) = 1234
				return nil
			},
			eachRows: [][]any{
				{int64(30), customTenant("t-1"), "c3", "u", 1, nil},
				{int64(20), customTenant("t-1"), "c2", "u", 1, nil},
				{int64(10), customTenant("t-1"), "c1", "u", 1, nil},
			},
		},
		onQuery: func(q string) { queries = append(queries, q) },
	}
	rows := tbl.Bind(sess)

	// CountNone skips COUNT(*) and returns -1.
	got, total, err := rows.List(t.Context(), nil, Listing{Count: CountNone})
	require.NoError(t, err)
	require.Equal(t, int64(-1), total)
	require.Len(t, got, 3)
	require.Len(t, queries, 1)

	// CountEstimate queries pg_class.reltuples.
	queries = nil
	_, total, err = rows.List(t.Context(), nil, Listing{Count: CountEstimate})
	require.NoError(t, err)
	require.Equal(t, int64(1234), total)
	require.Len(t, queries, 2)
	require.Contains(t, queries[0], "pg_class")

	// ListKeyset fetches Limit+1, sets HasMore=true, and builds tuple cursor.
	queries = nil
	page, err := rows.ListKeyset(t.Context(), nil, KeysetListing{
		SortColumn: "code",
		Direction:  "DESC",
		After:      []any{"c9", int64(99)},
		Limit:      2,
	})
	require.NoError(t, err)
	require.True(t, page.HasMore)
	require.Len(t, page.Items, 2)
	require.Equal(t, int64(30), page.Items[0].ID)
	require.Equal(t, int64(20), page.Items[1].ID)
	require.Contains(t, queries[0], "AND ((code, id) < ($2, $3)) ORDER BY code DESC, id DESC LIMIT $4")

	// Single-column keyset omits tuple parens.
	queries = nil
	page, err = rows.ListKeyset(t.Context(), nil, KeysetListing{
		Direction: "ASC",
		After:     []any{int64(5)},
		Limit:     5,
	})
	require.NoError(t, err)
	require.False(t, page.HasMore)
	require.Len(t, page.Items, 3)
	require.Contains(t, queries[0], "AND (id > $2) ORDER BY id ASC LIMIT $3")
}

type queryRecorderSession struct {
	fakeSession
	onQuery func(string)
}

func (q *queryRecorderSession) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	q.onQuery(query)
	return q.fakeSession.Scan(ctx, query, args, dest...)
}

func (q *queryRecorderSession) Each(ctx context.Context, query string, args []any, decode func(func(...any) error) error) error {
	q.onQuery(query)
	return q.fakeSession.Each(ctx, query, args, decode)
}

func TestViewNestedStructPrefixes(t *testing.T) {
	type orderPart struct {
		ID   int64  `db:"id"`
		Code string `db:"code"`
	}
	type customerPart struct {
		ID   int64  `db:"id"`
		Name string `db:"name,zero"`
	}
	type joinedRow struct {
		Order    orderPart    `db:"o_"`
		Customer customerPart `db:"c_"`
		Total    int64        `db:"total"`
	}

	reg := NewRegistry(RegistryConfig[customTenant]{})
	v := DefineView(reg, ViewSpec[joinedRow]{Name: "public.orders_with_customers"})
	require.Equal(t, "o_id, o_code, c_id, c_name, total", v.Cols())
	require.Equal(t,
		"o.id AS o_id, o.code AS o_code, c.id AS c_id, c.name AS c_name, total",
		v.SelectAs(map[string]string{"o_": "o", "c_": "c"}),
	)

	rec := &recordingSession{
		onScan: func(_ string, _ []any, dest ...any) error {
			*dest[0].(*int64) = 11
			*dest[1].(*string) = "ORD-11"
			*dest[2].(*int64) = 22
			require.NoError(t, dest[3].(*ZeroValue[string]).Scan("Acme"))
			*dest[4].(*int64) = 500
			return nil
		},
	}
	got, err := v.Bind(rec).One(t.Context(), "SELECT "+v.SelectAs(map[string]string{"o_": "o", "c_": "c"}))
	require.NoError(t, err)
	require.Equal(t, &joinedRow{
		Order:    orderPart{ID: 11, Code: "ORD-11"},
		Customer: customerPart{ID: 22, Name: "Acme"},
		Total:    500,
	}, got)
}

type fakeTracer struct {
	spans []struct {
		info TraceInfo
		rows int64
		err  error
	}
}

type traceCtxKey struct{}

func (f *fakeTracer) Start(ctx context.Context, info TraceInfo) (context.Context, func(int64, error)) {
	ctx = context.WithValue(ctx, traceCtxKey{}, info.Key)
	return ctx, func(rows int64, err error) {
		f.spans = append(f.spans, struct {
			info TraceInfo
			rows int64
			err  error
		}{info: info, rows: rows, err: err})
	}
}

type fakeMetrics struct {
	obs []struct {
		info     TraceInfo
		duration time.Duration
		rows     int64
		errClass ErrorClass
	}
}

func (m *fakeMetrics) Observe(_ context.Context, info TraceInfo, d time.Duration, rows int64, class ErrorClass) {
	m.obs = append(m.obs, struct {
		info     TraceInfo
		duration time.Duration
		rows     int64
		errClass ErrorClass
	}{info: info, duration: d, rows: rows, errClass: class})
}

func TestTracerAndMetricsHooks(t *testing.T) {
	tracer := &fakeTracer{}
	metrics := &fakeMetrics{}
	base := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	tick := 0
	reg := NewRegistry(RegistryConfig[customTenant]{
		ScopeID:  func(s customTenant) any { return string(s) },
		ScopeLog: func(s customTenant) string { return string(s) },
		Log: LogConfig{
			Now: func() time.Time {
				tick++
				return base.Add(time.Duration(tick) * 15 * time.Millisecond)
			},
		},
		Tracer:  tracer,
		Metrics: metrics,
	})
	tbl := Define(reg, TableSpec[customRow, int64]{Name: "public.custom_items"})

	var seenTraceKey any
	sess := &ctxCaptureSession{
		errSession: errSession{fakeSession: fakeSession{tenant: "t-7", execRows: 1}},
		onCtx:      func(ctx context.Context) { seenTraceKey = ctx.Value(traceCtxKey{}) },
	}
	rows := tbl.Bind(sess)

	require.NoError(t, rows.Insert(t.Context(), &customRow{Tenant: "t-7", Code: "ok", CreatedBy: "u", Version: 1}))
	require.Equal(t, "public.custom_items.insert", seenTraceKey)
	require.Len(t, tracer.spans, 1)
	require.Equal(t, TraceInfo{Key: "public.custom_items.insert", Table: "public.custom_items", Action: "insert", Scope: "t-7"}, tracer.spans[0].info)
	require.Equal(t, int64(1), tracer.spans[0].rows)
	require.NoError(t, tracer.spans[0].err)

	require.Len(t, metrics.obs, 1)
	require.Equal(t, 15*time.Millisecond, metrics.obs[0].duration)
	require.Equal(t, ErrorClass(""), metrics.obs[0].errClass)

	// Error populates ErrorClass on Metrics and err on Tracer.
	sess.scanErr = pgStateErr{"40P01", "deadlock"}
	_, err := rows.Get(t.Context(), 1)
	require.ErrorIs(t, err, ErrDeadlock)
	require.Len(t, metrics.obs, 2)
	require.Equal(t, ClassDeadlock, metrics.obs[1].errClass)
	require.Error(t, tracer.spans[1].err)
}

type ctxCaptureSession struct {
	errSession
	onCtx func(context.Context)
}

func (c *ctxCaptureSession) Returning(ctx context.Context, q string, args []any, dest ...any) error {
	c.onCtx(ctx)
	return c.errSession.Returning(ctx, q, args, dest...)
}
