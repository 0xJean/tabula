//go:build integration

package tabula_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/0xJean/tabula/stdsql"
)

// cents exercises C1 custom codec reuse on aggregate result types.
type cents int64

func (c cents) Value() (driver.Value, error) { return int64(c), nil }
func (c *cents) Scan(src any) error {
	switch v := src.(type) {
	case int64:
		*c = cents(v)
		return nil
	case []byte:
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return err
		}
		*c = cents(n)
		return nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return err
		}
		*c = cents(n)
		return nil
	default:
		return fmt.Errorf("unsupported cents scan type %T", src)
	}
}

func TestV031AggregatesComposedQueriesAndSQLFilesIntegration(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	ctx := t.Context()

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	schema := "tabula_c_it_" + suffix
	_, err = db.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })

	_, err = db.ExecContext(ctx, `
		CREATE TABLE `+schema+`.labels (
			id bigserial PRIMARY KEY,
			tenant_id text NOT NULL,
			caption text NOT NULL
		);
		CREATE TABLE `+schema+`.notes (
			id bigserial PRIMARY KEY,
			tenant_id text NOT NULL,
			label_id bigint REFERENCES `+schema+`.labels(id),
			code text NOT NULL,
			title text NOT NULL,
			category text NOT NULL,
			rank bigint NOT NULL,
			amount bigint NOT NULL,
			visible boolean NOT NULL DEFAULT true,
			pinned boolean NOT NULL DEFAULT false,
			due_at timestamptz NOT NULL,
			meta jsonb NOT NULL DEFAULT '{}'::jsonb,
			deleted_at timestamptz
		);
	`)
	require.NoError(t, err)

	type scope string
	type label struct {
		ID      int64  `db:"id,key,serial"`
		Tenant  scope  `db:"tenant_id,scope"`
		Caption string `db:"caption"`
	}
	type note struct {
		ID        int64           `db:"id,key,serial"`
		Tenant    scope           `db:"tenant_id,scope"`
		LabelID   *int64          `db:"label_id"`
		Code      string          `db:"code"`
		Title     string          `db:"title"`
		Category  string          `db:"category"`
		Rank      int64           `db:"rank"`
		Amount    cents           `db:"amount"`
		Visible   bool            `db:"visible"`
		Pinned    bool            `db:"pinned"`
		DueAt     time.Time       `db:"due_at"`
		Meta      json.RawMessage `db:"meta,json"`
		DeletedAt *time.Time      `db:"deleted_at,deleted"`
	}
	type noteView struct {
		ID      int64  `db:"id"`
		Rank    int64  `db:"rank"`
		Due     bool   `db:"due"`
		Caption string `db:"caption"`
	}

	key := tabula.NewSessionKey()
	reg := tabula.NewRegistry(tabula.RegistryConfig[scope]{
		RequireSealed: true,
		ScopeID:       func(s scope) any { return string(s) },
	})
	tabula.RegisterType(reg, func(p *cents) any { return p })
	labelsTbl := tabula.Define(reg, tabula.TableSpec[label, int64]{Name: schema + ".labels", Order: "id ASC"})
	notesTbl := tabula.Define(reg, tabula.TableSpec[note, int64]{
		Name:       schema + ".notes",
		Order:      "id ASC",
		GroupBy:    []string{"category"},
		Aggregates: tabula.ScalarAggregates{Max: []string{"rank"}, Min: []string{"rank"}, Sum: []string{"amount"}, Avg: []string{"amount"}},
	})

	// C4: embedded SQL files feed Select and CountBase.
	sqlFS := fstest.MapFS{
		"sql/notes_select.sql": &fstest.MapFile{Data: []byte(`
			SELECT n.id, n.rank, n.due_at <= $1 AS due, coalesce(l.caption, '') AS caption
			FROM ` + schema + `.notes n
			LEFT JOIN ` + schema + `.labels l ON l.id = n.label_id AND l.tenant_id = n.tenant_id AND $2::boolean
		`)},
		"sql/notes_count.sql": &fstest.MapFile{Data: []byte(`
			SELECT count(*) FROM ` + schema + `.notes n
		`)},
	}
	sqlFiles := tabula.MustSQLFiles(sqlFS, "sql")

	view := tabula.DefineView(reg, tabula.ViewSpec[noteView]{Name: schema + ".notes_view"})
	composed := view.DefineQuery("notes.search", tabula.QuerySpec{
		Select:    sqlFiles.Get("notes_select.sql"),
		CountBase: sqlFiles.Get("notes_count.sql"),
		Where:     "(n.visible OR n.pinned) AND n.deleted_at IS NULL",
		Scope:     tabula.ScopeColumns("n.tenant_id"),
		Filters: []tabula.QueryFilter{
			{Name: "q", SQL: "n.title ILIKE $1 OR n.code ILIKE $1", Secret: true},
			{Name: "category", SQL: "n.category = $1"},
			{Name: "meta_key", SQL: "n.meta ? $1"},
			{Name: "only_pinned", SQL: "n.pinned"},
		},
		Order: tabula.QueryOrderSpec{
			Default:          "rank",
			DefaultDirection: tabula.SortDesc,
			Sorts: []tabula.QuerySort{
				{Key: "rank", Expr: "n.rank", Column: "rank"},
			},
			TieBreaker: []tabula.QuerySortTerm{
				{Expr: "n.id", Column: "id"},
			},
		},
		MaxLimit:  10,
		MaxOffset: 20,
		Keyset:    true,
	})

	// Grouped query with Tail (GROUP BY / HAVING) + wrapped count.
	type catSummary struct {
		Category string `db:"category"`
		Total    int64  `db:"total"`
	}
	summaryView := tabula.DefineView(reg, tabula.ViewSpec[catSummary]{Name: schema + ".notes_summary"})
	summaryQuery := summaryView.DefineQuery("notes.by_cat", tabula.QuerySpec{
		Select: "SELECT n.category, count(*) AS total FROM " + schema + ".notes n",
		Where:  "n.deleted_at IS NULL",
		Tail:   "GROUP BY n.category HAVING count(*) >= $1",
		Scope:  tabula.ScopeColumns("n.tenant_id"),
		Order: tabula.QueryOrderSpec{
			Default:          "category",
			DefaultDirection: tabula.SortAsc,
			Sorts: []tabula.QuerySort{
				{Key: "category", Expr: "n.category", Column: "category"},
			},
		},
		MaxLimit: 10,
	})

	require.NoError(t, reg.Seal(key))

	// C3 + C1: VerifyStatements & VerifyViewStatements prepare ALL generated variants on real PG 17!
	tblFailures, err := sqlcheck.VerifyStatements(ctx, db, reg.Defined(), "")
	require.NoError(t, err)
	require.Empty(t, tblFailures)
	viewFailures, err := sqlcheck.VerifyViewStatements(ctx, db, reg.DefinedViews(), "")
	require.NoError(t, err)
	require.Empty(t, viewFailures)

	runner, err := stdsql.NewRunner(db, stdsql.ScopePolicy[scope]{
		IsUnscoped: func(s scope) bool { return s == "all" },
	}, stdsql.RunnerOptions{
		Unscoped:   stdsql.UnscopedReadWrite,
		SessionKey: key,
	})
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)

	// 1. C1 on empty table: SQL NULL -> (zero, false, nil); CountBy -> empty map; Exists -> false.
	require.NoError(t, runner.Within(ctx, "t1", func(txCtx context.Context, s *stdsql.Session[scope]) error {
		r := notesTbl.Bind(s)
		peak, ok, err := tabula.Max[int64](txCtx, r, "rank", nil)
		require.NoError(t, err)
		require.False(t, ok)
		require.Zero(t, peak)

		sumVal, ok, err := tabula.Sum[cents](txCtx, r, "amount", nil)
		require.NoError(t, err)
		require.False(t, ok)
		require.Zero(t, sumVal)

		counts, err := tabula.CountBy[string](txCtx, r, "category", nil)
		require.NoError(t, err)
		require.Empty(t, counts)

		exists, err := r.Exists(txCtx, nil)
		require.NoError(t, err)
		require.False(t, exists)
		return nil
	}))

	// 2. Seed two tenants including zero-amount row, tied rank values, JSONB keys, and soft-deleted row.
	require.NoError(t, runner.Within(ctx, "t1", func(txCtx context.Context, s *stdsql.Session[scope]) error {
		lbl := label{Tenant: "t1", Caption: "Urgent"}
		require.NoError(t, labelsTbl.Bind(s).Insert(txCtx, &lbl))
		r := notesTbl.Bind(s)
		seed := []note{
			{Tenant: "t1", LabelID: &lbl.ID, Code: "N-01", Title: "Alpha task", Category: "ops", Rank: 10, Amount: 0, Visible: true, DueAt: now.Add(-time.Hour), Meta: json.RawMessage(`{"tag":"v1"}`)},
			{Tenant: "t1", LabelID: &lbl.ID, Code: "N-02", Title: "Beta task", Category: "ops", Rank: 10, Amount: 250, Visible: true, Pinned: true, DueAt: now.Add(time.Hour), Meta: json.RawMessage(`{"tag":"v2","priority":"high"}`)},
			{Tenant: "t1", Code: "N-03", Title: "Gamma alpha", Category: "eng", Rank: 5, Amount: 150, Visible: true, DueAt: now.Add(-time.Minute), Meta: json.RawMessage(`{"priority":"low"}`)},
			{Tenant: "t1", Code: "N-04", Title: "Hidden", Category: "eng", Rank: 1, Amount: 50, Visible: false, DueAt: now, Meta: json.RawMessage(`{}`)},
		}
		for i := range seed {
			require.NoError(t, r.Insert(txCtx, &seed[i]))
		}
		// Soft-delete an extra row to verify aggregates exclude deleted rows.
		delRow := note{Tenant: "t1", Code: "N-DEL", Title: "Deleted", Category: "ops", Rank: 999, Amount: 9999, Visible: true, DueAt: now, Meta: json.RawMessage(`{}`)}
		require.NoError(t, r.Insert(txCtx, &delRow))
		delRow.DeletedAt = &now
		require.NoError(t, r.SoftDelete(txCtx, delRow))
		return nil
	}))
	require.NoError(t, runner.Within(ctx, "t2", func(txCtx context.Context, s *stdsql.Session[scope]) error {
		other := note{Tenant: "t2", Code: "T2-1", Title: "Alpha other tenant", Category: "ops", Rank: 500, Amount: 5000, Visible: true, DueAt: now, Meta: json.RawMessage(`{"priority":"high"}`)}
		return notesTbl.Bind(s).Insert(txCtx, &other)
	}))

	// 3. C1 aggregates with data (genuine zero vs NULL, custom codec, CountBy, Exists, Rows.ListKeyset Next).
	require.NoError(t, runner.WithinReadOnly(ctx, "t1", func(txCtx context.Context, s *stdsql.Session[scope]) error {
		r := notesTbl.Bind(s)

		// Genuine zero returns present=true.
		minAmt, ok, err := tabula.Min[cents](txCtx, r, "amount", nil)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, cents(0), minAmt)

		sumAmt, ok, err := tabula.Sum[cents](txCtx, r, "amount", func(f *tabula.Filter) { f.And("category = ?", "ops") })
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, cents(250), sumAmt)

		maxRank, ok, err := tabula.Max[int64](txCtx, r, "rank", nil)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, int64(10), maxRank)

		avgRank, ok, err := tabula.Avg[float64](txCtx, r, "rank", func(f *tabula.Filter) { f.And("visible = ?", true) })
		require.NoError(t, err)
		require.True(t, ok)
		require.InDelta(t, 25.0/3.0, avgRank, 0.01)

		counts, err := tabula.CountBy[string](txCtx, r, "category", nil)
		require.NoError(t, err)
		require.Equal(t, map[string]int64{"ops": 2, "eng": 2}, counts)

		ok, err = r.Exists(txCtx, func(f *tabula.Filter) { f.And("pinned = ?", true) })
		require.NoError(t, err)
		require.True(t, ok)

		// Rows.ListKeyset auto-populates KeysetPage.Next for continuous paging.
		page1, err := r.ListKeyset(txCtx, nil, tabula.KeysetListing{Limit: 2})
		require.NoError(t, err)
		require.Len(t, page1.Items, 2)
		require.True(t, page1.HasMore)
		require.Equal(t, []any{page1.Items[1].ID}, page1.Next)

		page2, err := r.ListKeyset(txCtx, nil, tabula.KeysetListing{Limit: 2, After: page1.Next})
		require.NoError(t, err)
		require.Len(t, page2.Items, 2)
		require.False(t, page2.HasMore)
		require.Nil(t, page2.Next)

		// 4. C2 Composed query: 1-value multi-column ILIKE, JSONB ?, CountBase remapping, tied-key ListKeyset!
		q := composed.Bind(s)

		// Multi-column search reusing $1 + projection/JOIN base args ($1=now, $2=true).
		pSearch := tabula.NewQueryParams(now, true).With("q", "%alpha%")
		items, total, err := q.List(txCtx, pSearch, tabula.QueryPage{Size: 10})
		require.NoError(t, err)
		require.Equal(t, int64(2), total)
		require.Len(t, items, 2)
		require.Equal(t, "Urgent", items[0].Caption)
		require.True(t, items[0].Due)

		// JSONB ? operator round-trip without escaping.
		pJSON := tabula.NewQueryParams(now, true).With("meta_key", "priority")
		jsonCount, err := q.Count(txCtx, pJSON)
		require.NoError(t, err)
		require.Equal(t, int64(2), jsonCount)

		// Zero-arg toggle filter.
		pPinned := tabula.NewQueryParams(now, true).With("only_pinned")
		pinnedItems, err := q.Find(txCtx, pPinned, tabula.QueryOrder{})
		require.NoError(t, err)
		require.Len(t, pinnedItems, 1)
		require.False(t, pinnedItems[0].Due)

		// Tied rank values (two rows with rank=10, one with rank=5): continuous keyset paging with limit=1!
		pAll := tabula.NewQueryParams(now, true)
		_, _, err = q.List(txCtx, pAll, tabula.QueryPage{Size: 1, Offset: 21})
		require.ErrorContains(t, err, "MaxOffset")
		last, _, err := q.List(txCtx, pAll, tabula.QueryPage{Size: 1, Offset: 20, Count: tabula.CountNone})
		require.NoError(t, err)
		require.Empty(t, last)
		var pagedIDs []int64
		var cursor []any
		for {
			kp, err := q.ListKeyset(txCtx, pAll, tabula.QueryKeyset{Limit: 1, After: cursor})
			require.NoError(t, err)
			require.Len(t, kp.Items, 1)
			pagedIDs = append(pagedIDs, kp.Items[0].ID)
			if !kp.HasMore {
				require.Nil(t, kp.Next)
				break
			}
			require.Len(t, kp.Next, 2)
			cursor = kp.Next
		}
		require.Len(t, pagedIDs, 3)

		// Iter with early exit closes cursor cleanly.
		var iterCount int
		for _, iterErr := range q.Iter(txCtx, pAll, tabula.QueryOrder{}) {
			require.NoError(t, iterErr)
			iterCount++
			break
		}
		require.Equal(t, 1, iterCount)

		// Grouped query with Tail + wrapped count.
		sq := summaryQuery.Bind(s)
		groups, groupTotal, err := sq.List(txCtx, tabula.NewQueryParams(2), tabula.QueryPage{Size: 5})
		require.NoError(t, err)
		require.Equal(t, int64(2), groupTotal)
		require.Len(t, groups, 2)
		return nil
	}))

	// 5. Unscoped session omits ScopeColumns predicate and sees both tenants.
	require.NoError(t, runner.WithinReadOnly(ctx, "all", func(txCtx context.Context, s *stdsql.Session[scope]) error {
		total, err := composed.Bind(s).Count(txCtx, tabula.NewQueryParams(now, true))
		require.NoError(t, err)
		require.Equal(t, int64(4), total)
		return nil
	}))
}

func TestComposedKeysetDirectionPostgres(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	runner, err := stdsql.NewRunner(db, stdsql.NoScopePolicy(), stdsql.RunnerOptions{})
	require.NoError(t, err)

	type row struct {
		ID   int64 `db:"id"`
		Rank int64 `db:"rank"`
	}
	asc := []int64{2, 4, 1, 3}
	desc := []int64{3, 1, 4, 2}
	for _, tc := range []struct {
		name           string
		fixed, request tabula.SortDirection
		want           []int64
	}{
		{"fixed ascending, descending request", tabula.SortAsc, tabula.SortDesc, asc},
		{"fixed descending, ascending request", tabula.SortDesc, tabula.SortAsc, desc},
		{"fixed ascending, default request", tabula.SortAsc, tabula.SortDefault, asc},
		{"caller selects ascending", tabula.SortDefault, tabula.SortAsc, asc},
		{"caller selects descending", tabula.SortDefault, tabula.SortDesc, desc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
			view := tabula.DefineView(reg, tabula.ViewSpec[row]{Name: "ranked"})
			query := view.DefineQuery("page", tabula.QuerySpec{
				Select: "SELECT id, rank FROM (VALUES (1::bigint,20::bigint),(2,10),(3,20),(4,10)) AS v(id,rank)",
				Scope:  tabula.ScopeGlobal(),
				Order: tabula.QueryOrderSpec{
					Default:          "rank",
					DefaultDirection: tabula.SortDesc,
					Sorts:            []tabula.QuerySort{{Key: "rank", Expr: "rank", Column: "rank", Direction: tc.fixed}},
					TieBreaker:       []tabula.QuerySortTerm{{Expr: "id", Column: "id", Direction: tc.fixed}},
				},
				Keyset:   true,
				MaxLimit: 1,
			})
			require.NoError(t, runner.WithinReadOnly(t.Context(), struct{}{}, func(ctx context.Context, s *stdsql.Session[struct{}]) error {
				rows := query.Bind(s)
				request := tabula.QueryKeyset{Limit: 1, Direction: tc.request}
				for i, id := range tc.want {
					page, err := rows.ListKeyset(ctx, nil, request)
					require.NoError(t, err)
					require.Len(t, page.Items, 1)
					item := page.Items[0]
					require.Equal(t, id, item.ID)
					require.Equal(t, i+1 < len(tc.want), page.HasMore)
					if page.HasMore {
						require.Equal(t, []any{item.Rank, item.ID}, page.Next)
					} else {
						require.Nil(t, page.Next)
					}
					request.After = page.Next
				}
				return nil
			}))
		})
	}
}
