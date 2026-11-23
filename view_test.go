package tabula

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

type projectionRow struct {
	ID    int64    `db:"id"`
	Code  string   `db:"code"`
	Note  string   `db:"note,zero"`
	Tags  []string `db:"tags,array"`
	Extra string   `db:"-"`
}

func TestViewDefineColsAndSelect(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{
		ScopeID: func(s customTenant) any { return string(s) },
		ArrayColumn: func(ft reflect.Type) (func(any) any, error) {
			return func(p any) any { return p }, nil
		},
	})
	v := DefineView(reg, ViewSpec[projectionRow]{Name: "public.join_view"})
	require.Equal(t, "id, code, note, tags", v.Cols())
	require.Equal(t, "c.id, c.code, c.note, c.tags", v.ColsAs("c"))
	require.Equal(t, "id, code, note, tags", v.ColsAs(""))

	stmt := v.Statement("by_code", "SELECT "+v.ColsAs("c")+" FROM public.join_view c WHERE c.code = $1")
	require.Equal(t, stmt, v.Definition().Statements["by_code"])
	require.Len(t, reg.DefinedViews(), 1)
	require.Empty(t, reg.Defined(), "views stay separate from table Defined()")

	// Invalid declarations fail cleanly.
	_, err := TryDefineView(reg, ViewSpec[projectionRow]{})
	require.EqualError(t, err, "tabula.DefineView: name is required")
	type badRole struct {
		ID int64 `db:"id,key"`
	}
	_, err = TryDefineView(reg, ViewSpec[badRole]{Name: "v"})
	require.EqualError(t, err, "tabula.DefineView v: column id: option key is not supported on a View")

	// Verify Bind + One/Select/Scan dispatch SQL cleanly.
	var scannedQuery string
	rec := &recordingSession{
		onScan: func(query string, _ []any, dest ...any) error {
			scannedQuery = query
			*dest[0].(*int64) = 9
			*dest[1].(*string) = "SKU-1"
			require.NoError(t, dest[2].(*ZeroValue[string]).Scan(nil))
			*dest[3].(*[]string) = []string{"r1"}
			return nil
		},
		onEach: func(query string, _ []any, scan func(...any) error) error {
			scannedQuery = query
			return scan(func(dest ...any) error {
				*dest[0].(*int64) = 7
				*dest[1].(*string) = "SKU-2"
				require.NoError(t, dest[2].(*ZeroValue[string]).Scan("primary"))
				*dest[3].(*[]string) = []string{"https://example.invalid"}
				return nil
			})
		},
	}
	one, err := v.Bind(rec).One(t.Context(), stmt, "SKU-1")
	require.NoError(t, err)
	require.Equal(t, stmt, scannedQuery)
	require.Equal(t, &projectionRow{ID: 9, Code: "SKU-1", Note: "", Tags: []string{"r1"}}, one)

	list, err := v.Bind(rec).Select(t.Context(), stmt, "SKU-2")
	require.NoError(t, err)
	require.Equal(t, []projectionRow{{ID: 7, Code: "SKU-2", Note: "primary", Tags: []string{"https://example.invalid"}}}, list)
}

func TestLockByGetManyAndLockMany(t *testing.T) {
	var wrappedSlices []any
	reg := NewRegistry(RegistryConfig[customTenant]{
		ScopeID: func(s customTenant) any { return string(s) },
		ArrayArg: func(v any) any {
			wrappedSlices = append(wrappedSlices, v)
			return v
		},
	})
	tbl := Define(reg, TableSpec[customRow, int64]{Name: "public.custom_items", Order: "id DESC"})
	sess := &fakeSession{
		tenant:   "t-1",
		execRows: 1,
		scanFn: func(dest ...any) error {
			*dest[0].(*int64) = 10
			*dest[1].(*customTenant) = "t-1"
			*dest[2].(*string) = "beta"
			*dest[3].(*string) = "bob"
			*dest[4].(*int) = 1
			return nil
		},
		eachRows: [][]any{
			{int64(10), customTenant("t-1"), "beta", "bob", 1, nil},
			{int64(20), customTenant("t-1"), "gamma", "carol", 2, nil},
		},
	}
	rows := tbl.Bind(sess)

	// Empty GetMany / LockMany issue no SQL.
	got, err := rows.GetMany(t.Context(), nil)
	require.NoError(t, err)
	require.Nil(t, got)
	require.Empty(t, sess.lastQuery)
	got, err = rows.LockMany(t.Context(), []int64{})
	require.NoError(t, err)
	require.Nil(t, got)
	require.Empty(t, sess.lastQuery)

	// LockBy applies scope + LIMIT 1 FOR UPDATE and remembers once columns.
	locked, err := rows.LockBy(t.Context(), "code = ?", "beta")
	require.NoError(t, err)
	require.Equal(t, int64(10), locked.ID)
	require.Contains(t, sess.lastQuery, "AND (tenant_id = $1) AND (deleted_at IS NULL) AND (code = $2) ORDER BY id DESC LIMIT 1 FOR UPDATE")
	locked.CreatedBy = "tampered"
	require.ErrorIs(t, rows.Save(t.Context(), *locked), ErrOnce)

	// GetMany uses table order + ANY($n) through ArrayArg.
	got, err = rows.GetMany(t.Context(), []int64{20, 10})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Contains(t, sess.lastQuery, "AND (id = ANY($2)) ORDER BY id DESC")
	require.Equal(t, []int64{20, 10}, wrappedSlices[0])

	// LockMany forces deterministic ORDER BY key ASC FOR UPDATE and remembers once columns.
	lockedBatch, err := rows.LockMany(t.Context(), []int64{20, 10})
	require.NoError(t, err)
	require.Len(t, lockedBatch, 2)
	require.Contains(t, sess.lastQuery, "AND (id = ANY($2)) ORDER BY id ASC FOR UPDATE")
	lockedBatch[1].CreatedBy = "tampered"
	require.ErrorIs(t, rows.Save(t.Context(), lockedBatch[1]), ErrOnce)
}

type recordingSession struct {
	fakeSession
	onScan func(query string, args []any, dest ...any) error
	onEach func(query string, args []any, fn func(...any) error) error
}

func (r *recordingSession) Scan(_ context.Context, query string, args []any, dest ...any) error {
	return r.onScan(query, args, dest...)
}

func (r *recordingSession) Each(_ context.Context, query string, args []any, visit func(func(...any) error) error) error {
	return r.onEach(query, args, func(fn ...any) error {
		return visit(fn[0].(func(...any) error))
	})
}
