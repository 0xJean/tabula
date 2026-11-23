package tabula

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type customTenant string

type customRow struct {
	ID        int64        `db:"id,key,serial"`
	Tenant    customTenant `db:"tenant_id,scope"`
	Code      string       `db:"code"`
	CreatedBy string       `db:"created_by,once"`
	Version   int          `db:"version,version"`
	DeletedAt *time.Time   `db:"deleted_at,deleted"`
}

type fakeSession struct {
	tenant   customTenant
	unscoped bool

	lastQuery string
	lastArgs  []any
	execRows  int64
	scanFn    func(dest ...any) error
	eachRows  [][]any
}

func (f *fakeSession) Scope() customTenant { return f.tenant }
func (f *fakeSession) Unscoped() bool      { return f.unscoped }
func (f *fakeSession) Exec(_ context.Context, query string, args ...any) (int64, error) {
	f.lastQuery, f.lastArgs = query, args
	return f.execRows, nil
}
func (f *fakeSession) Scan(_ context.Context, query string, args []any, dest ...any) error {
	f.lastQuery, f.lastArgs = query, args
	if f.scanFn != nil {
		return f.scanFn(dest...)
	}
	return nil
}
func (f *fakeSession) Returning(_ context.Context, query string, args []any, dest ...any) error {
	f.lastQuery, f.lastArgs = query, args
	if f.scanFn != nil {
		return f.scanFn(dest...)
	}
	return nil
}
func (f *fakeSession) Each(_ context.Context, query string, args []any, decode func(func(...any) error) error) error {
	f.lastQuery, f.lastArgs = query, args
	for _, r := range f.eachRows {
		if err := decode(func(dest ...any) error {
			for i := range dest {
				switch d := dest[i].(type) {
				case *int64:
					*d = r[i].(int64)
				case *customTenant:
					*d = r[i].(customTenant)
				case *string:
					*d = r[i].(string)
				case *int:
					*d = r[i].(int)
				case **time.Time:
					if r[i] != nil {
						*d = r[i].(*time.Time)
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func TestStandaloneRegistryAndRowsFlow(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{
		ScopeID: func(s customTenant) any { return string(s) },
	})
	tbl := Define(reg, TableSpec[customRow, int64]{
		Name:  "public.custom_items",
		Order: "id ASC",
	})
	tbl.Statement("by_code", "SELECT "+tbl.Cols()+" FROM public.custom_items WHERE code = $1")
	tbl.Requires("public.custom_items", "SELECT", "id")

	defs := reg.Defined()
	require.Len(t, defs, 1)
	require.Equal(t, tbl.Definition(), defs[0])
	require.Contains(t, defs[0].Statements, "by_code")

	sess := &fakeSession{
		tenant:   "t-1",
		execRows: 1,
		scanFn: func(dest ...any) error {
			switch d := dest[0].(type) {
			case *bool:
				*d = true
			case *int64:
				*d = 42
				if len(dest) > 1 {
					*dest[1].(*customTenant) = "t-1"
					*dest[2].(*string) = "alpha"
					*dest[3].(*string) = "alice"
					*dest[4].(*int) = 1
				}
			}
			return nil
		},
		eachRows: [][]any{
			{int64(42), customTenant("t-1"), "alpha", "alice", 1, nil},
		},
	}

	rows := tbl.Bind(sess)

	// ExistsBy automatically applies tenant + soft-delete scope.
	ok, err := rows.ExistsBy(t.Context(), "code = ?", "alpha")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "SELECT EXISTS (SELECT 1 FROM public.custom_items WHERE true AND (tenant_id = $1) AND (deleted_at IS NULL) AND (code = $2))", sess.lastQuery)
	require.Equal(t, []any{"t-1", "alpha"}, sess.lastArgs)

	// Find skips COUNT(*) and applies order + paging.
	got, err := rows.Find(t.Context(), func(f *Filter) {
		f.And("code = ?", "alpha")
	}, Listing{Order: "id DESC", Size: 10, Offset: 0})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int64(42), got[0].ID)
	require.Contains(t, sess.lastQuery, "ORDER BY id DESC LIMIT $3 OFFSET $4")

	// Lock remembers once columns; Save refuses mutated once column with ErrOnce.
	locked, err := rows.Lock(t.Context(), 42)
	require.NoError(t, err)
	require.Equal(t, "alice", locked.CreatedBy)

	locked.CreatedBy = "mallory"
	locked.Version = 2
	require.ErrorIs(t, rows.Save(t.Context(), *locked), ErrOnce)

	// Restoring once column allows Save with version CAS.
	locked.CreatedBy = "alice"
	require.NoError(t, rows.Save(t.Context(), *locked))
	require.Contains(t, sess.lastQuery, "AND version = $4 - 1")

	// Zero rows updated maps to ErrConflict.
	sess.execRows = 0
	require.ErrorIs(t, rows.Save(t.Context(), *locked), ErrConflict)
}

func TestFilterAndOrderSpecStandalone(t *testing.T) {
	f := NewFilter("t1")
	f.And("payload ?? ? AND name = ?", "k", "n")
	require.Equal(t, " AND (payload ? $2 AND name = $3)", f.Where())
	require.Equal(t, []any{"t1", "k", "n"}, f.Args())

	spec := MustOrderSpec(OrderSpec{
		Columns:          map[string]string{"code": "code", "id": "id"},
		DefaultColumn:    "id",
		DefaultDirection: "ASC",
		TieBreaker:       "id ASC",
	})
	require.Equal(t, "code DESC, id ASC", spec.Resolve("code", "desc"))
	require.Equal(t, "id ASC", spec.Resolve("bad; --", "weird"))
}

func TestDescriptorOwnsCompiledConfiguration(t *testing.T) {
	type model struct {
		ID     int64  `db:"id,key"`
		Status string `db:"status,deleted"`
		Extra  string `db:"-"`
	}
	reg := NewRegistry(RegistryConfig[struct{}]{})
	spec := TableSpec[model, int64]{Name: "items", ReadOnly: true, SoftDelete: &SoftDelete{DeletedValue: "gone"}, Columns: []Column[model]{Col("extra", func(row *model) any { return &row.Extra })}}
	table := Define(reg, spec)
	spec.Name = "changed"
	spec.ReadOnly = false
	spec.SoftDelete.DeletedValue = "other"
	spec.Columns[0].Name = "new"
	d := table.Definition()
	d.Privileges[0].Columns[0] = "corrupt"
	delete(d.Statements, "get")
	require.Equal(t, "items", table.Name())
	require.True(t, table.ReadOnly())
	require.Equal(t, "id, status, extra", table.Cols())
	require.Equal(t, "SELECT id, status, extra FROM items WHERE id = $1 AND status <> 'gone'", table.Definition().Statements["get"])
	require.Equal(t, "id", reg.Defined()[0].Privileges[0].Columns[0])

	type projection struct {
		Name string `db:"-"`
	}
	viewSpec := ViewSpec[projection]{Name: "v", Columns: []Column[projection]{Col("name", func(r *projection) any { return &r.Name })}}
	v := DefineView(reg, viewSpec)
	viewSpec.Columns[0].Addr = nil
	viewSpec.Name = "other"
	require.Equal(t, "v", v.Name())
	require.Equal(t, "name", v.Cols())
}

type batchSession struct {
	fakeSession
	params []int
	failAt int
}

func (s *batchSession) Exec(_ context.Context, q string, args ...any) (int64, error) {
	s.params = append(s.params, len(args))
	if s.failAt == len(s.params) {
		return 0, errors.New("chunk failed")
	}
	return int64(len(args) / 3), nil
}

func TestUpsertManyParameterBoundaryAndScope(t *testing.T) {
	type row struct {
		ID     int64        `db:"id,key"`
		Tenant customTenant `db:"tenant_id,scope"`
		Value  int          `db:"v"`
	}
	reg := NewRegistry(RegistryConfig[customTenant]{ScopeID: func(s customTenant) any { return []byte(s) }})
	tbl := Define(reg, TableSpec[row, int64]{Name: "items"})
	p := tbl.DefineUpsert("up", UpsertSpec{Conflict: []string{"id"}, Update: []string{"v"}, Returning: []string{"v"}})
	require.Contains(t, p.SQL(), "WHERE items.tenant_id = EXCLUDED.tenant_id")
	sess := &batchSession{fakeSession: fakeSession{tenant: "a"}}
	r := tbl.Bind(sess)
	rows := make([]row, 65535/3+1)
	for i := range rows {
		rows[i] = row{ID: int64(i), Tenant: "a"}
	}
	n, err := r.UpsertMany(t.Context(), p, nil)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Empty(t, sess.params)
	rows[len(rows)-1].Tenant = "b"
	_, err = r.UpsertMany(t.Context(), p, rows)
	require.ErrorIs(t, err, ErrScopeMismatch)
	require.Empty(t, sess.params)
	rows[len(rows)-1].Tenant = "a"
	n, err = r.UpsertMany(t.Context(), p, rows)
	require.NoError(t, err)
	require.EqualValues(t, len(rows), n)
	require.Equal(t, []int{65535, 3}, sess.params)
	sess.params = nil
	sess.failAt = 2
	n, err = r.UpsertMany(t.Context(), p, rows)
	require.ErrorContains(t, err, "chunk failed")
	require.EqualValues(t, len(rows)-1, n)
}

func TestLockedIteratorSharesOnceGuardWithLogger(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	tbl := Define(reg, TableSpec[customRow, int64]{Name: "items"})
	s := &fakeSession{tenant: "t", eachRows: [][]any{{int64(1), customTenant("t"), "code", "owner", 1, nil}}}
	r := tbl.Bind(s)
	for v, err := range r.Iter(t.Context(), nil, Listing{Lock: LockSkipLocked}) {
		require.NoError(t, err)
		v.CreatedBy = "changed"
		require.ErrorIs(t, r.WithLogger(slog.Default()).Save(t.Context(), v), ErrOnce)
		break
	}
	require.Contains(t, s.lastQuery, "FOR UPDATE SKIP LOCKED")
}
