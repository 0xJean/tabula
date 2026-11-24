package tabula

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGuardNoSessionEscape(t *testing.T) {
	blocked := []string{
		"SET ROLE postgres",
		"set local role admin_user",
		"SET SESSION ROLE foo",
		"RESET ROLE",
		"reset all",
		"DISCARD ALL",
		"SELECT 1; /* reset connection */ DISCARD ALL",
		`DO $$BEGIN NULL; END;$$`,
		`SELECT E'one; two'; /* next statement */ dO LANGUAGE plpgsql $body$BEGIN NULL; END;$body$`,
		"SET SESSION AUTHORIZATION DEFAULT",
		"RESET SESSION AUTHORIZATION",
		"SELECT set_config('app.current_tenant', 'other', true)",
		"SELECT pg_catalog.set_config('role', 'postgres', false)",
		"SELECT 1; /* sneaky */ SET LOCAL ROLE super",
		`SELECT "set_config"('x','y',true)`,
		`SELECT U&"lab\0065l" FROM items`,
		`SELECT E'\''; SET ROLE postgres`,
		`SELECT e'\''; RESET ALL`,
		`SELECT E'\\'; SET SESSION AUTHORIZATION DEFAULT`,
	}
	for _, q := range blocked {
		require.ErrorIs(t, GuardNoSessionEscape(q), ErrForbiddenStatement, "should block: %s", q)
	}

	allowed := []string{
		"SELECT id, role FROM public.users WHERE id = $1",
		"UPDATE public.users SET role = $2 WHERE id = $1",
		"SELECT 'SET ROLE postgres' AS literal_note FROM public.items",
		"SELECT * FROM public.items WHERE note = 'don''t call set_config(1)'",
		`SELECT E'don\'t; SET ROLE postgres' AS note`,
		`SELECT $body$; DISCARD ALL; U&"label"$body$`,
		`SELECT $body$; DO nothing$body$ AS "do" /* DO */`,
		"INSERT INTO items (id) VALUES (1) ON CONFLICT DO NOTHING",
		"INSERT INTO items (id) VALUES (1) ON CONFLICT (id) DO UPDATE SET id = excluded.id",
		`SELECT u & "mask" FROM items`,
		"-- SET ROLE postgres\nSELECT id FROM public.items",
		"/* RESET ROLE; set_config('a','b',true) */ SELECT 1",
	}
	for _, q := range allowed {
		require.NoError(t, GuardNoSessionEscape(q), "should allow: %s", q)
	}
}

func TestGuardReadOnlyStatementAndSessionChecks(t *testing.T) {
	for _, q := range []string{"DISCARD ALL", "SELECT 1; DISCARD ALL", "DELETE FROM items", `DO $$BEGIN NULL; END;$$`} {
		require.ErrorIs(t, GuardReadOnly(q), ErrReadOnly, q)
	}
	for _, q := range []string{`SELECT U&"label" FROM items`, "WITH x AS (SELECT set_config('scope.id','a',true)) SELECT * FROM x"} {
		require.ErrorIs(t, GuardReadOnly(q), ErrForbiddenStatement, q)
	}
	for _, q := range []string{`SELECT 1 AS ";" FROM items;`, `SELECT $$DISCARD ALL$$`, `SELECT u & "mask" FROM items`} {
		require.NoError(t, GuardReadOnly(q), q)
	}
}

func TestReadOnlyAndPhysicalDelete(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{
		ScopeID: func(s customTenant) any { return string(s) },
	})

	// ReadOnly + AllowDelete cannot combine.
	_, err := TryDefine(reg, TableSpec[customRow, int64]{
		Name:        "public.bad",
		ReadOnly:    true,
		AllowDelete: true,
	})
	require.ErrorContains(t, err, "ReadOnly cannot combine with AllowDelete")

	// ReadOnly table derives only read statements & SELECT privilege, refuses writes.
	ro := Define(reg, TableSpec[customRow, int64]{
		Name:     "public.readonly_items",
		ReadOnly: true,
	})
	roDef := ro.Definition()
	require.Contains(t, roDef.Statements, "get")
	require.Contains(t, roDef.Statements, "list")
	require.Contains(t, roDef.Statements, "count")
	require.NotContains(t, roDef.Statements, "insert")
	require.NotContains(t, roDef.Statements, "update")
	require.NotContains(t, roDef.Statements, "delete")
	require.Len(t, roDef.Privileges, 1)
	require.Equal(t, "SELECT", roDef.Privileges[0].Privilege)
	_, err = ro.TryDefineUpsert("u", UpsertSpec{Conflict: []string{"id"}})
	require.ErrorContains(t, err, "ReadOnly")

	sess := &fakeSession{tenant: "t-1", execRows: 1}
	roRows := ro.Bind(sess)
	row := customRow{ID: 1, Tenant: "t-1", Code: "a", CreatedBy: "u", Version: 1}
	require.ErrorIs(t, roRows.Insert(t.Context(), &row), ErrReadOnly)
	require.ErrorIs(t, roRows.InsertReturning(t.Context(), &row), ErrReadOnly)
	require.ErrorIs(t, roRows.InsertMany(t.Context(), []customRow{row}), ErrReadOnly)
	require.ErrorIs(t, roRows.Save(t.Context(), row), ErrReadOnly)
	require.ErrorIs(t, roRows.SaveReturning(t.Context(), &row), ErrReadOnly)
	require.ErrorIs(t, roRows.Set(t.Context(), row, "code"), ErrReadOnly)
	_, err = roRows.UpdateWhere(t.Context(), row, "true")
	require.ErrorIs(t, err, ErrReadOnly)
	_, err = roRows.SetWhere(t.Context(), row, []string{"code"}, "true")
	require.ErrorIs(t, err, ErrReadOnly)
	require.ErrorIs(t, roRows.Delete(t.Context(), 1), ErrReadOnly)
	_, err = roRows.DeleteWhere(t.Context(), "id = ?", 1)
	require.ErrorIs(t, err, ErrReadOnly)

	// AllowDelete derives delete statements & DELETE privilege.
	del := Define(reg, TableSpec[customRow, int64]{
		Name:        "public.ephemeral_items",
		AllowDelete: true,
	})
	del.RequiresFunction("public.cleanup_item(bigint)")
	delDef := del.Definition()
	require.Equal(t, "DELETE FROM public.ephemeral_items WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL", delDef.Statements["delete"])
	require.Equal(t, "DELETE FROM public.ephemeral_items WHERE id = $1 AND deleted_at IS NULL", delDef.Statements["unscoped_delete"])
	require.Contains(t, delDef.Privileges, DefinedPrivilege{Table: "public.ephemeral_items", Privilege: "DELETE"})
	require.Contains(t, delDef.Privileges, DefinedPrivilege{Privilege: "EXECUTE", Function: "public.cleanup_item(bigint)"})

	delRows := del.Bind(sess)
	require.NoError(t, delRows.Delete(t.Context(), 42))
	require.Equal(t, delDef.Statements["delete"], sess.lastQuery)
	require.Len(t, sess.lastArgs, 2)
	require.Equal(t, int64(42), *sess.lastArgs[0].(*int64))
	require.Equal(t, "t-1", sess.lastArgs[1])

	sess.execRows = 0
	require.ErrorIs(t, delRows.Delete(t.Context(), 42), ErrConflict)

	sess.execRows = 3
	n, err := delRows.DeleteWhere(t.Context(), "code = ?", "expired")
	require.NoError(t, err)
	require.Equal(t, int64(3), n)
	require.Equal(t, "DELETE FROM public.ephemeral_items WHERE true AND (tenant_id = $1) AND (deleted_at IS NULL) AND (code = $2)", sess.lastQuery)

	// Guard against accidental full-table delete or table without AllowDelete.
	require.Panics(t, func() { _, _ = delRows.DeleteWhere(t.Context(), "   ") })
	noDel := Define(reg, TableSpec[customRow, int64]{Name: "public.no_delete_items"})
	require.Panics(t, func() { _ = noDel.Bind(sess).Delete(t.Context(), 1) })
}
