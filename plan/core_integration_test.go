//go:build integration

package plan_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/0xJean/tabula/stdsql"
	"github.com/stretchr/testify/require"
)

func TestTypedByRLSRole(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn, "TABULA_TEST_DSN is required for integration tests")
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx := t.Context()
	suffix := time.Now().UnixNano()
	schema := fmt.Sprintf("tabula_shape_%d", suffix)
	role := fmt.Sprintf("scope_reader_%d", suffix)
	table := schema + ".items"
	exec := func(sql string) {
		t.Helper()
		_, err := db.ExecContext(ctx, sql)
		require.NoError(t, err)
	}

	exec("CREATE ROLE " + role + " NOLOGIN NOBYPASSRLS")
	t.Cleanup(func() { _, err := db.Exec("DROP ROLE " + role); require.NoError(t, err) })
	exec("CREATE SCHEMA " + schema)
	t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + schema + " CASCADE"); require.NoError(t, err) })
	exec("CREATE TABLE " + table + " (id bigint PRIMARY KEY, group_id bigint NOT NULL, label text)")
	exec("INSERT INTO " + table + " (id, group_id) VALUES (1, 10), (2, 20)")
	exec("GRANT USAGE ON SCHEMA " + schema + " TO " + role)
	exec("GRANT SELECT(id) ON " + table + " TO " + role)
	exec("ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY")
	exec("ALTER TABLE " + table + " FORCE ROW LEVEL SECURITY")
	exec("CREATE POLICY scoped_rows ON " + table + " USING (group_id = current_setting('demo.scope')::bigint)")

	def, authority, bind, err := plan.RLSReadFixture(table)
	require.NoError(t, err)
	key := "by_id"
	shape := def.StatementShapes[key]
	require.Len(t, shape.Inputs, 1)
	require.False(t, shape.Inputs[0].Scope)
	require.NotContains(t, def.Statements[key], "group_id")
	require.NotContains(t, def.Statements[key], "$2")
	grants, findings := sqlcheck.CollectPrivilegeNeeds([]tabula.Definition{{Name: table, Privileges: shape.Privileges}})
	require.Empty(t, findings)
	check := sqlcheck.ReadinessNeeds{Privileges: grants, AnyColumnSelect: shape.AnyColumnSelect,
		SchemaUsage: shape.SchemaUsage, ForceRLS: shape.ForceRLS}
	missing, err := sqlcheck.CheckReadiness(ctx, db, role, check)
	require.NoError(t, err)
	require.Empty(t, missing)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(ctx, "SET LOCAL ROLE "+role)
	require.NoError(t, err)
	require.Len(t, shape.PhysicalColumns, 1)
	col := shape.PhysicalColumns[0]
	var sameType bool
	err = tx.QueryRowContext(ctx, `SELECT atttypid = pg_catalog.to_regtype($3)
FROM pg_catalog.pg_attribute
WHERE attrelid = pg_catalog.to_regclass($1) AND attname = $2 AND NOT attisdropped`, col.Relation, col.Name, col.Type).Scan(&sameType)
	require.NoError(t, err)
	require.True(t, sameType)
	require.NoError(t, tx.Rollback())
	find := func(scope string, request int64) (int64, error) {
		t.Helper()
		runner, err := stdsql.NewRunner(db, stdsql.ScopePolicy[struct{}]{
			ManagedVars: []string{"demo.scope"},
			ScopeVars:   func(struct{}) map[string]string { return map[string]string{"demo.scope": scope} },
		}, stdsql.RunnerOptions{SQLAuthority: authority, Role: role})
		require.NoError(t, err)
		var id int64
		err = runner.WithinReadOnly(ctx, struct{}{}, func(c context.Context, s *stdsql.Session[struct{}]) error {
			call, err := bind(s, request)
			require.NoError(t, err)
			view, err := authority.Open(s, tabula.CallScan, call)
			require.NoError(t, err)
			require.Equal(t, []any{request}, view.Args)
			require.ErrorIs(t, s.Scan(c, view.Query, view.Args, &id), tabula.ErrLegacySQL)
			require.ErrorIs(t, s.EachAuthorized(c, call, nil), tabula.ErrStatementAuthority)
			return s.ScanAuthorized(c, call, &id)
		})
		return id, err
	}
	id, err := find("10", 1)
	require.NoError(t, err)
	require.EqualValues(t, 1, id)
	_, err = find("10", 2)
	require.ErrorIs(t, err, sql.ErrNoRows)
	id, err = find("20", 2)
	require.NoError(t, err)
	require.EqualValues(t, 2, id)

	exec("ALTER TABLE " + table + " NO FORCE ROW LEVEL SECURITY")
	missing, err = sqlcheck.CheckReadiness(ctx, db, role, check)
	require.NoError(t, err)
	require.Len(t, missing, 1)
	require.Equal(t, "FORCE RLS", missing[0].Privilege)
}

func TestTypedReturningStreamUsesWritePolicy(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx := t.Context()
	table := fmt.Sprintf("public.tabula_entries_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE TABLE "+table+" (id bigint primary key, label text)")
	require.NoError(t, err)
	t.Cleanup(func() { _, err := db.Exec("DROP TABLE " + table); require.NoError(t, err) })
	_, err = db.ExecContext(ctx, "INSERT INTO "+table+" VALUES (1,'first'),(2,'second')")
	require.NoError(t, err)
	authority, bind, err := plan.ReturningRowsFixture(table)
	require.NoError(t, err)
	runner := func(policy tabula.StatementPolicy) *stdsql.Runner[struct{}] {
		r, err := stdsql.NewRunner(db, stdsql.NoScopePolicy(), stdsql.RunnerOptions{SQLAuthority: authority, Statements: policy})
		require.NoError(t, err)
		return r
	}
	scope := struct{}{}
	read := runner(tabula.StatementPolicy{Read: []string{"select"}})
	require.NoError(t, read.Within(ctx, scope, func(c context.Context, s *stdsql.Session[struct{}]) error {
		call, err := bind(s)
		require.NoError(t, err)
		require.ErrorIs(t, s.EachAuthorized(c, call, nil), tabula.ErrForbiddenStatement)
		return nil
	}))
	write := runner(tabula.StatementPolicy{Write: []string{"update"}, RequireReturning: true})
	require.NoError(t, write.WithinReadOnly(ctx, scope, func(_ context.Context, s *stdsql.Session[struct{}]) error {
		_, err := bind(s)
		require.ErrorIs(t, err, tabula.ErrReadOnly)
		return nil
	}))
	require.NoError(t, write.Within(ctx, scope, func(c context.Context, s *stdsql.Session[struct{}]) error {
		call, err := bind(s)
		require.NoError(t, err)
		require.ErrorIs(t, s.ScanAuthorized(c, call), tabula.ErrStatementAuthority)
		var ids []int64
		err = s.EachAuthorized(c, call, func(scan func(...any) error) error {
			var id int64
			if err := scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
			return nil
		})
		require.NoError(t, err)
		require.ElementsMatch(t, []int64{1, 2}, ids)
		return nil
	}))
}
