//go:build integration

package sqlcheck_test

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula/sqlcheck"
)

func TestCheckLoginRole(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	ctx := t.Context()

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	schema := "tabula_lr_" + suffix
	loginRole := "tabula_login_" + suffix
	workerRole := "tabula_worker_" + suffix
	extraRole := "tabula_extra_" + suffix

	mustExec := func(q string) {
		t.Helper()
		_, err := db.ExecContext(ctx, q)
		require.NoError(t, err, q)
	}

	mustExec(`CREATE SCHEMA ` + schema)
	t.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`) })

	mustExec(`CREATE ROLE ` + workerRole + ` NOLOGIN`)
	mustExec(`CREATE ROLE ` + extraRole + ` NOLOGIN`)
	mustExec(`CREATE ROLE ` + loginRole + ` LOGIN NOINHERIT PASSWORD 'test-only'`)
	mustExec(`GRANT ` + workerRole + ` TO ` + loginRole)
	mustExec(`REVOKE CREATE ON SCHEMA ` + schema + ` FROM PUBLIC`)
	mustExec(`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + loginRole)
	mustExec(`CREATE TABLE ` + schema + `.items (id int PRIMARY KEY, note text)`)
	t.Cleanup(func() {
		_, _ = db.Exec(`REVOKE ALL ON SCHEMA ` + schema + ` FROM ` + loginRole)
		_, _ = db.Exec(`REVOKE ALL ON ALL TABLES IN SCHEMA ` + schema + ` FROM ` + loginRole)
		_, _ = db.Exec(`DROP ROLE IF EXISTS ` + loginRole)
		_, _ = db.Exec(`DROP ROLE IF EXISTS ` + workerRole)
		_, _ = db.Exec(`DROP ROLE IF EXISTS ` + extraRole)
	})

	// 1. Clean login role passes.
	failures, err := sqlcheck.CheckLoginRole(ctx, db, sqlcheck.LoginRoleExpectation{
		Role:     loginRole,
		MemberOf: []string{workerRole},
		Schemas:  []string{schema},
	})
	require.NoError(t, err)
	require.Empty(t, failures)

	// 2. Missing role reports not found.
	failures, err = sqlcheck.CheckLoginRole(ctx, db, sqlcheck.LoginRoleExpectation{
		Role:    "no_such_role_" + suffix,
		Schemas: []string{schema},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"role no_such_role_" + suffix + ": not found"}, failures)

	// 3. Introduce multiple violations and verify all are reported.
	mustExec(`ALTER ROLE ` + loginRole + ` INHERIT BYPASSRLS CREATEROLE CREATEDB REPLICATION`)
	mustExec(`GRANT ` + extraRole + ` TO ` + loginRole)
	mustExec(`GRANT SELECT ON ` + schema + `.items TO ` + loginRole)
	mustExec(`GRANT UPDATE (note) ON ` + schema + `.items TO ` + loginRole)
	mustExec(`GRANT CREATE ON SCHEMA ` + schema + ` TO ` + loginRole)
	mustExec(`CREATE TABLE ` + schema + `.owned_by_login (id int)`)
	mustExec(`ALTER TABLE ` + schema + `.owned_by_login OWNER TO ` + loginRole)

	failures, err = sqlcheck.CheckLoginRole(ctx, db, sqlcheck.LoginRoleExpectation{
		Role:     loginRole,
		MemberOf: []string{workerRole, "missing_role"},
		Schemas:  []string{schema, "missing_schema_" + suffix},
	})
	require.NoError(t, err)
	joined := strings.Join(failures, "\n")
	require.Contains(t, joined, "must be NOINHERIT")
	require.Contains(t, joined, "BYPASSRLS")
	require.Contains(t, joined, "CREATEROLE")
	require.Contains(t, joined, "CREATEDB")
	require.Contains(t, joined, "REPLICATION")
	require.Contains(t, joined, "unexpected membership in "+extraRole)
	require.Contains(t, joined, "missing membership in missing_role")
	require.Contains(t, joined, "holds direct SELECT privilege on "+schema+".items")
	require.Contains(t, joined, "holds direct UPDATE privilege on "+schema+".items(note)")
	require.Contains(t, joined, "owns relation "+schema+".owned_by_login")
	require.Contains(t, joined, "has CREATE privilege on schema "+schema)
	require.Contains(t, joined, "schema missing_schema_"+suffix+": not found")

	// Clean ownership before DROP ROLE in cleanup.
	mustExec(`DROP TABLE ` + schema + `.owned_by_login`)

	// Validation errors.
	_, err = sqlcheck.CheckLoginRole(ctx, db, sqlcheck.LoginRoleExpectation{})
	require.Error(t, err)
}
