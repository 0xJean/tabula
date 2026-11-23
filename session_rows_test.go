package tabula

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

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
		Tenant customTenant `db:"tenant_id,tenant"`
		Value  int          `db:"v"`
	}
	reg := NewRegistry(RegistryConfig[customTenant]{TenantID: func(s customTenant) any { return []byte(s) }})
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
