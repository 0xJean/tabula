//go:build integration

package postgresfixture_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xJean/tabula/postgresfixture"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Compare names on the disposable cluster so the check also works when other
// tests have already populated the cache.
func testClonesAndLogins(t *testing.T, admin *sql.DB) []string {
	t.Helper()
	var names pq.StringArray
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT ARRAY(
		SELECT datname::text FROM pg_catalog.pg_database WHERE starts_with(datname::text, 'tabula_test_')
		UNION ALL
		SELECT rolname::text FROM pg_catalog.pg_roles WHERE starts_with(rolname::text, 'tabula_login_')
		ORDER BY 1)`).Scan(&names))
	return names
}

func TestPrepareColdStartAndReuse(t *testing.T) {
	admin, f := loginFixture(t)
	ctx := t.Context()
	member := "fixture_reader_" + rand.Text()
	quoted := pq.QuoteIdentifier(member)
	plan := postgresfixture.Installation{
		Revision: t.TempDir(),
		Base:     []byte("CREATE TABLE notes (id integer); INSERT INTO notes VALUES (7)"),
		Migrations: []postgresfixture.Migration{
			{Name: "001_reader.sql", Content: []byte("CREATE ROLE " + quoted + " NOLOGIN; GRANT SELECT ON notes TO " + quoted)},
		},
	}
	template, err := plan.TemplateName()
	require.NoError(t, err)
	require.False(t, cloneExists(t, admin, template))
	require.False(t, roleExists(t, admin, member))
	t.Cleanup(func() {
		_, err := admin.Exec("DROP ROLE IF EXISTS " + quoted)
		require.NoError(t, err)
	})
	cleanupDatabase(t, admin, template)

	// Catch even a transient per-test copy that gets dropped before Prepare returns.
	setup := f
	setup.Backend = inspectCopy{f.Backend, template, func(context.Context, *sql.Conn) error {
		return errors.New("unexpected physical copy from the published template")
	}}
	before := testClonesAndLogins(t, admin)
	require.ErrorContains(t, setup.Prepare(ctx, postgresfixture.Installation{}), "installation revision")
	require.NoError(t, setup.Prepare(ctx, plan))
	require.True(t, roleExists(t, admin, member))
	require.Equal(t, before, testClonesAndLogins(t, admin))

	var originalOID int64
	require.NoError(t, admin.QueryRowContext(ctx, "SELECT oid FROM pg_catalog.pg_database WHERE datname=$1", template).Scan(&originalOID))
	const prefix = "tabula fixture template; last used "
	const old = prefix + "2000-01-01T00:00:00Z"
	_, err = admin.ExecContext(ctx, "COMMENT ON DATABASE "+pq.QuoteIdentifier(template)+" IS "+pq.QuoteLiteral(old))
	require.NoError(t, err)
	// A second install would also fail on the migration's CREATE ROLE.
	require.NoError(t, setup.Prepare(ctx, plan))
	var oid int64
	require.NoError(t, admin.QueryRowContext(ctx, "SELECT oid FROM pg_catalog.pg_database WHERE datname=$1", template).Scan(&oid))
	require.Equal(t, originalOID, oid)
	mark := cloneComment(t, admin, template)
	require.NotEqual(t, old, mark)
	stamp, ok := strings.CutPrefix(mark, prefix)
	require.True(t, ok)
	_, err = time.Parse(time.RFC3339Nano, stamp)
	require.NoError(t, err)
	_, err = f.CloneWithLease(ctx, plan, nil)
	require.ErrorContains(t, err, "requires a fixture login")
	require.Equal(t, before, testClonesAndLogins(t, admin))

	login, err := f.AcquireLoginRole(ctx, postgresfixture.LoginRoleOptions{MemberOf: []string{member}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, login.Release(context.WithoutCancel(ctx))) })
	db, err := f.CloneWithLease(ctx, plan, login)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Drop(context.WithoutCancel(ctx))) })
	client, err := loginClient(t, db.Name(), login).Conn(ctx)
	require.NoError(t, err)
	defer client.Close()
	_, err = client.ExecContext(ctx, "SET ROLE "+quoted)
	require.NoError(t, err)
	var id int
	require.NoError(t, client.QueryRowContext(ctx, "SELECT id FROM notes").Scan(&id))
	require.Equal(t, 7, id)
}

func TestPrepareConcurrentPublicationAndPrune(t *testing.T) {
	admin, f := loginFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	plan := postgresfixture.Installation{Revision: t.TempDir(), Base: []byte("CREATE TABLE notes (id integer)")}
	template, err := plan.TemplateName()
	require.NoError(t, err)
	stage := "tabula_stg_" + strings.TrimPrefix(template, "tabula_tpl_")
	cleanupDatabase(t, admin, template)
	cleanupDatabase(t, admin, stage)
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(stage)+" TEMPLATE template0")
	require.NoError(t, err)
	backend := f.Backend.(*installer)
	abandoned, err := backend.open(stage)
	require.NoError(t, err)
	// Reusing this interrupted build would conflict with the installation.
	_, err = abandoned.ExecContext(ctx, "CREATE TABLE notes (partial text)")
	require.NoError(t, abandoned.Close())
	require.NoError(t, err)

	// Hold the stage lock until both Prepare calls are waiting to publish.
	gate, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	var wg sync.WaitGroup
	defer func() {
		_ = gate.Rollback()
		wg.Wait()
	}()
	var holder int
	require.NoError(t, gate.QueryRowContext(ctx, "SELECT pg_catalog.pg_backend_pid()").Scan(&holder))
	_, err = gate.ExecContext(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1,0))", stage)
	require.NoError(t, err)
	var results [2]error
	for i := range results {
		wg.Go(func() { results[i] = f.Prepare(ctx, plan) })
	}
	require.Eventually(t, func() bool {
		var waiting int
		err := admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
			WHERE $1::integer = ANY(pg_catalog.pg_blocking_pids(pid))`, holder).Scan(&waiting)
		return err == nil && waiting == len(results)
	}, 10*time.Second, 10*time.Millisecond)

	dropped, err := f.Prune(ctx, time.Hour)
	require.NoError(t, err)
	require.NotContains(t, dropped, stage)
	require.True(t, cloneExists(t, admin, stage))
	require.NoError(t, gate.Rollback())
	wg.Wait()
	for _, err := range results {
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), backend.installs.Load())
	require.True(t, cloneExists(t, admin, template))
	require.False(t, cloneExists(t, admin, stage))

	// Once both callers have returned, Prune can acquire the template lock.
	_, err = admin.ExecContext(ctx, "COMMENT ON DATABASE "+pq.QuoteIdentifier(template)+" IS 'tabula fixture template; last used 2000-01-01T00:00:00Z'")
	require.NoError(t, err)
	dropped, err = f.Prune(ctx, time.Hour)
	require.NoError(t, err)
	require.Contains(t, dropped, template)
}
