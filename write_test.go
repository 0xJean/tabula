package tabula

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpdateWhereAndSetWhere(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{
		ScopeID: func(s customTenant) any { return string(s) },
	})
	tbl := Define(reg, TableSpec[customRow, int64]{Name: "public.custom_items", Order: "id ASC"})
	sess := &fakeSession{
		tenant:   "t-1",
		execRows: 1,
		scanFn: func(dest ...any) error {
			*dest[0].(*int64) = 10
			*dest[1].(*customTenant) = "t-1"
			*dest[2].(*string) = "pending"
			*dest[3].(*string) = "alice"
			*dest[4].(*int) = 1
			return nil
		},
	}
	rows := tbl.Bind(sess)

	// Lock remembers once column; UpdateWhere enforces it.
	locked, err := rows.Lock(t.Context(), 10)
	require.NoError(t, err)
	locked.CreatedBy = "mallory"
	locked.Version = 2
	matched, err := rows.UpdateWhere(t.Context(), *locked, "code = ?", "pending")
	require.False(t, matched)
	require.ErrorIs(t, err, ErrOnce)

	// Valid UpdateWhere appends condition with renumbered placeholders & ?? escape.
	locked.CreatedBy = "alice"
	locked.Code = "approved"
	matched, err = rows.UpdateWhere(t.Context(), *locked, "code = ? AND meta ?? ?", "pending", "k")
	require.NoError(t, err)
	require.True(t, matched)
	require.Contains(t, sess.lastQuery, "AND version = $4 - 1 AND (code = $6 AND meta ? $7)")

	// Zero rows updated is NOT an error for UpdateWhere or SetWhere.
	sess.execRows = 0
	matched, err = rows.UpdateWhere(t.Context(), *locked, "code = ?", "pending")
	require.NoError(t, err)
	require.False(t, matched)

	matched, err = rows.SetWhere(t.Context(), *locked, []string{"code"}, "code = ?", "pending")
	require.NoError(t, err)
	require.False(t, matched)
	require.Equal(t, "UPDATE public.custom_items SET code = $3 WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL AND (code = $4)", sess.lastQuery)

	// SetWhere reports true when 1 row matched.
	sess.execRows = 1
	matched, err = rows.SetWhere(t.Context(), *locked, []string{"code"}, "code = ?", "pending")
	require.NoError(t, err)
	require.True(t, matched)

	// SetWhere panics on once, key, tenant, or unknown columns.
	require.Panics(t, func() { _, _ = rows.SetWhere(t.Context(), *locked, []string{"created_by"}, "true") })
	require.Panics(t, func() { _, _ = rows.SetWhere(t.Context(), *locked, []string{"id"}, "true") })
	require.Panics(t, func() { _, _ = rows.SetWhere(t.Context(), *locked, []string{"tenant_id"}, "true") })
	require.Panics(t, func() { _, _ = rows.SetWhere(t.Context(), *locked, []string{"missing"}, "true") })
}

func TestControlledUpsert(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{
		ScopeID: func(s customTenant) any { return string(s) },
	})
	tbl := Define(reg, TableSpec[customRow, int64]{Name: "public.custom_items"})

	// Validation rejects key, scope, once, empty conflict, unknown cols, Where on DO NOTHING.
	for _, tc := range []struct {
		name string
		spec UpsertSpec
		want string
	}{
		{"empty_conflict", UpsertSpec{}, "conflict columns are required"},
		{"unknown_conflict", UpsertSpec{Conflict: []string{"nope"}}, "conflict column nope is not declared"},
		{"dup_conflict", UpsertSpec{Conflict: []string{"code", "code"}}, "conflict column code declared twice"},
		{"where_do_nothing", UpsertSpec{Conflict: []string{"code"}, Where: "true"}, "Where requires at least one Update column"},
		{"update_key", UpsertSpec{Conflict: []string{"code"}, Update: []string{"id"}}, "column id (key) cannot be updated on conflict"},
		{"update_scope", UpsertSpec{Conflict: []string{"code"}, Update: []string{"tenant_id"}}, "column tenant_id (scope) cannot be updated on conflict"},
		{"update_once", UpsertSpec{Conflict: []string{"code"}, Update: []string{"created_by"}}, "insert-only column created_by cannot be updated on conflict"},
		{"unknown_returning", UpsertSpec{Conflict: []string{"code"}, Returning: []string{"ghost"}}, "returning column ghost is not declared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tbl.TryDefineUpsert("bad", tc.spec)
			require.ErrorContains(t, err, tc.want)
		})
	}

	doNothing := tbl.DefineUpsert("upsert_ignore", UpsertSpec{
		Conflict: []string{"tenant_id", "code"},
	})
	require.Equal(t,
		"INSERT INTO public.custom_items (tenant_id, code, created_by, version) VALUES ($1, $2, $3, $4) ON CONFLICT (tenant_id, code) DO NOTHING",
		doNothing.SQL(),
	)

	doUpdate := tbl.DefineUpsert("upsert_code", UpsertSpec{
		Conflict:  []string{"tenant_id", "code"},
		Update:    []string{"version"},
		Where:     "public.custom_items.deleted_at IS NULL",
		Returning: []string{"id", "version"},
	})
	require.Equal(t,
		"INSERT INTO public.custom_items (tenant_id, code, created_by, version) VALUES ($1, $2, $3, $4) ON CONFLICT (tenant_id, code) DO UPDATE SET version = EXCLUDED.version WHERE public.custom_items.deleted_at IS NULL RETURNING id, version",
		doUpdate.SQL(),
	)
	require.Contains(t, tbl.Definition().Statements, "upsert_ignore")
	require.Contains(t, tbl.Definition().Statements, "upsert_code")

	sess := &fakeSession{tenant: "t-1", execRows: 0}
	rows := tbl.Bind(sess)
	row := customRow{Tenant: "t-1", Code: "alpha", CreatedBy: "alice", Version: 1}

	// DO NOTHING returns false when skipped.
	ok, err := rows.Upsert(t.Context(), doNothing, &row)
	require.NoError(t, err)
	require.False(t, ok)

	// DO UPDATE with RETURNING populates row fields.
	sess.scanFn = func(dest ...any) error {
		*dest[0].(*int64) = 99
		*dest[1].(*int) = 2
		return nil
	}
	ok, err = rows.Upsert(t.Context(), doUpdate, &row)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(99), row.ID)
	require.Equal(t, 2, row.Version)

	// DO UPDATE with WHERE skipping row returns sql.ErrNoRows -> (false, nil).
	sess.scanFn = func(dest ...any) error { return sql.ErrNoRows }
	ok, err = rows.Upsert(t.Context(), doUpdate, &row)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestInsertReturningSaveReturningAndInsertMany(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{
		ScopeID: func(s customTenant) any { return string(s) },
	})
	tbl := Define(reg, TableSpec[customRow, int64]{Name: "public.custom_items"})
	sess := &fakeSession{
		tenant:   "t-1",
		execRows: 2,
		scanFn: func(dest ...any) error {
			*dest[0].(*int64) = 77
			*dest[1].(*int) = 3
			return nil
		},
	}
	rows := tbl.Bind(sess)

	row := customRow{Tenant: "t-1", Code: "beta", CreatedBy: "bob", Version: 1}
	require.NoError(t, rows.InsertReturning(t.Context(), &row, "id", "version"))
	require.Equal(t, int64(77), row.ID)
	require.Equal(t, 3, row.Version)
	require.Equal(t,
		"INSERT INTO public.custom_items (tenant_id, code, created_by, version) VALUES ($1, $2, $3, $4) RETURNING id, version",
		sess.lastQuery,
	)

	// SaveReturning maps sql.ErrNoRows to ErrConflict.
	sess.scanFn = func(dest ...any) error { return sql.ErrNoRows }
	require.ErrorIs(t, rows.SaveReturning(t.Context(), &row, "version"), ErrConflict)
	require.Contains(t, sess.lastQuery, "RETURNING version")

	// InsertMany empty is a no-op.
	sess.lastQuery = ""
	require.NoError(t, rows.InsertMany(t.Context(), nil))
	require.Empty(t, sess.lastQuery)

	// InsertMany rejects mixed tenants in a tenant-scoped session.
	err := rows.InsertMany(t.Context(), []customRow{
		{Tenant: "t-1", Code: "a", CreatedBy: "u", Version: 1},
		{Tenant: "t-2", Code: "b", CreatedBy: "u", Version: 1},
	})
	require.ErrorIs(t, err, ErrScopeMismatch)

	// InsertMany generates multi-row VALUES with sequential placeholders.
	require.NoError(t, rows.InsertMany(t.Context(), []customRow{
		{Tenant: "t-1", Code: "a", CreatedBy: "u", Version: 1},
		{Tenant: "t-1", Code: "b", CreatedBy: "u", Version: 1},
	}))
	require.Equal(t,
		"INSERT INTO public.custom_items (tenant_id, code, created_by, version) VALUES ($1, $2, $3, $4), ($5, $6, $7, $8)",
		sess.lastQuery,
	)
	require.Len(t, sess.lastArgs, 8)
}

type pairKey struct {
	Region string `db:"region_code"`
	SKU    string `db:"sku"`
}

type pairRow struct {
	Region    string       `db:"region_code,key"`
	SKU       string       `db:"sku,key"`
	Tenant    customTenant `db:"tenant_id,scope"`
	Priority  int          `db:"priority"`
	CreatedBy string       `db:"created_by,once"`
}

func TestCompositeKeyTable(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{
		ScopeID: func(s customTenant) any { return string(s) },
	})
	tbl := Define(reg, TableSpec[pairRow, pairKey]{
		Name:  "public.region_items",
		Order: "region_code ASC, sku ASC",
	})
	def := tbl.Definition()
	require.Equal(t, "SELECT region_code, sku, tenant_id, priority, created_by FROM public.region_items WHERE region_code = $1 AND sku = $2 AND tenant_id = $3", def.Statements["get"])
	require.Equal(t, "UPDATE public.region_items SET priority = $4 WHERE region_code = $1 AND sku = $2 AND tenant_id = $3", def.Statements["update"])

	sess := &fakeSession{
		tenant:   "t-1",
		execRows: 1,
		scanFn: func(dest ...any) error {
			*dest[0].(*string) = "US"
			*dest[1].(*string) = "WIDGET"
			*dest[2].(*customTenant) = "t-1"
			*dest[3].(*int) = 6
			*dest[4].(*string) = "alice"
			return nil
		},
		eachRows: [][]any{
			{"US", "WIDGET", customTenant("t-1"), 6, "alice"},
		},
	}
	rows := tbl.Bind(sess)

	// Lock + once tracking works with composite struct key.
	locked, err := rows.Lock(t.Context(), pairKey{Region: "US", SKU: "WIDGET"})
	require.NoError(t, err)
	require.Equal(t, "US", locked.Region)
	require.Equal(t, "WIDGET", locked.SKU)
	locked.CreatedBy = "tampered"
	require.ErrorIs(t, rows.Save(t.Context(), *locked), ErrOnce)

	// GetMany & LockMany use PostgreSQL tuple IN and composite ORDER BY ASC.
	_, err = rows.GetMany(t.Context(), []pairKey{
		{Region: "US", SKU: "WIDGET"},
		{Region: "EU", SKU: "GADGET"},
	})
	require.NoError(t, err)
	require.Contains(t, sess.lastQuery, "AND ((region_code, sku) IN (($2, $3), ($4, $5))) ORDER BY region_code ASC, sku ASC")

	_, err = rows.LockMany(t.Context(), []pairKey{{Region: "US", SKU: "WIDGET"}})
	require.NoError(t, err)
	require.Contains(t, sess.lastQuery, "ORDER BY region_code ASC, sku ASC FOR UPDATE")

	// Reject mismatched composite key struct.
	type badKey struct {
		Region string `db:"region_code"`
	}
	_, err = TryDefine(reg, TableSpec[pairRow, badKey]{Name: "public.bad"})
	require.ErrorContains(t, err, "missing key column sku")
}

func TestConfigurableSoftDelete(t *testing.T) {
	reg := NewRegistry(RegistryConfig[struct{}]{})

	t.Run("boolean_deleted", func(t *testing.T) {
		type boolRow struct {
			ID        int64  `db:"id,key"`
			Name      string `db:"name"`
			IsDeleted bool   `db:"is_deleted,deleted"`
		}
		tbl := Define(reg, TableSpec[boolRow, int64]{Name: "public.bool_items"})
		require.Equal(t, "SELECT id, name, is_deleted FROM public.bool_items WHERE id = $1 AND is_deleted = FALSE", tbl.Definition().Statements["get"])
		require.Equal(t, "INSERT INTO public.bool_items (id, name, is_deleted) VALUES ($1, $2, $3)", tbl.Definition().Statements["insert"])

		sess := &noopSession{execRows: 1}
		rows := tbl.Bind(sess)
		require.ErrorIs(t, rows.SoftDelete(t.Context(), boolRow{ID: 1, Name: "a", IsDeleted: false}), ErrNotDeleted)
		require.NoError(t, rows.SoftDelete(t.Context(), boolRow{ID: 1, Name: "a", IsDeleted: true}))
	})

	t.Run("status_deleted_not_equal", func(t *testing.T) {
		type statusRow struct {
			ID     int64  `db:"id,key"`
			Status string `db:"status,deleted"`
		}
		tbl := Define(reg, TableSpec[statusRow, int64]{
			Name:       "public.status_items",
			SoftDelete: &SoftDelete{DeletedValue: "deleted"},
		})
		require.Equal(t, "SELECT id, status FROM public.status_items WHERE id = $1 AND status <> 'deleted'", tbl.Definition().Statements["get"])

		sess := &noopSession{execRows: 1}
		rows := tbl.Bind(sess)
		require.ErrorIs(t, rows.SoftDelete(t.Context(), statusRow{ID: 1, Status: "active"}), ErrNotDeleted)
		require.NoError(t, rows.SoftDelete(t.Context(), statusRow{ID: 1, Status: "deleted"}))
	})

	t.Run("status_alive_and_deleted_int", func(t *testing.T) {
		type intStatusRow struct {
			ID    int64 `db:"id,key"`
			State int   `db:"state,deleted"`
		}
		tbl := Define(reg, TableSpec[intStatusRow, int64]{
			Name:       "public.int_status_items",
			SoftDelete: &SoftDelete{AliveValue: 1, DeletedValue: 9},
		})
		require.Equal(t, "SELECT id, state FROM public.int_status_items WHERE id = $1 AND state = 1", tbl.Definition().Statements["get"])

		sess := &noopSession{execRows: 1}
		rows := tbl.Bind(sess)
		require.ErrorIs(t, rows.SoftDelete(t.Context(), intStatusRow{ID: 1, State: 1}), ErrNotDeleted)
		require.NoError(t, rows.SoftDelete(t.Context(), intStatusRow{ID: 1, State: 9}))
	})

	t.Run("invalid_soft_delete_configs", func(t *testing.T) {
		type statusRow struct {
			ID     int64  `db:"id,key"`
			Status string `db:"status,deleted"`
		}
		_, err := TryDefine(reg, TableSpec[statusRow, int64]{Name: "public.s"})
		require.ErrorContains(t, err, "requires SoftDelete.DeletedValue")

		_, err = TryDefine(reg, TableSpec[statusRow, int64]{
			Name:       "public.s",
			SoftDelete: &SoftDelete{AliveValue: "x", DeletedValue: "x"},
		})
		require.ErrorContains(t, err, "must differ")
	})
}

type noopSession struct {
	execRows int64
	lastSQL  string
}

func (n *noopSession) Scope() struct{} { return struct{}{} }
func (n *noopSession) Unscoped() bool  { return false }
func (n *noopSession) Exec(_ context.Context, q string, _ ...any) (int64, error) {
	n.lastSQL = q
	return n.execRows, nil
}
func (n *noopSession) Scan(_ context.Context, q string, _ []any, _ ...any) error {
	n.lastSQL = q
	return nil
}
func (n *noopSession) Returning(_ context.Context, q string, _ []any, _ ...any) error {
	n.lastSQL = q
	return nil
}
func (n *noopSession) Each(_ context.Context, q string, _ []any, _ func(func(...any) error) error) error {
	n.lastSQL = q
	return nil
}
