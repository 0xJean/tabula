//go:build integration

package postgresfixture_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0xJean/tabula/postgresfixture"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func cloneExists(t *testing.T, admin *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, admin.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT FROM pg_catalog.pg_database WHERE datname=$1)", name).Scan(&exists))
	return exists
}

func cloneComment(t *testing.T, admin *sql.DB, name string) string {
	t.Helper()
	var mark string
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT coalesce(pg_catalog.shobj_description(oid,'pg_database'),'')
		FROM pg_catalog.pg_database WHERE datname=$1`, name).Scan(&mark))
	return mark
}

func cleanupDatabase(t *testing.T, admin *sql.DB, name string) {
	t.Helper()
	t.Cleanup(func() {
		_, err := admin.Exec("DROP DATABASE IF EXISTS " + pq.QuoteIdentifier(name))
		require.NoError(t, err)
	})
}

func waitForNoClients(t *testing.T, admin *sql.DB, name string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var idle bool
		err := admin.QueryRowContext(t.Context(), "SELECT NOT EXISTS(SELECT FROM pg_catalog.pg_stat_activity WHERE datname=$1)", name).Scan(&idle)
		return err == nil && idle
	}, 5*time.Second, 10*time.Millisecond)
}

func TestLeasedClonesAfterProcessDeath(t *testing.T) {
	admin, f := loginFixture(t)
	ctx := t.Context()
	member := fmt.Sprintf("tabula_member_%d", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, "CREATE ROLE "+member)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = f.PruneLoginRoles(context.Background(), 0)
		_, _ = admin.Exec("DROP ROLE " + member)
	})
	plan := postgresfixture.Installation{Revision: member, Base: []byte("SELECT 1")}
	template, err := plan.TemplateName()
	require.NoError(t, err)
	cleanupDatabase(t, admin, template)
	a, b := startLoginWorker(t, member), startLoginWorker(t, member)
	roleA, roleB := a.read(t), b.read(t)
	newClone := func(worker *loginWorker) string {
		t.Helper()
		_, err := fmt.Fprintln(worker.input, "clone")
		require.NoError(t, err)
		name := worker.read(t)
		require.Regexp(t, "^tabula_test_[0-9a-f]{24}$", name)
		cleanupDatabase(t, admin, name)
		return name
	}
	live, orphan := newClone(a), newClone(b)
	require.True(t, strings.HasSuffix(cloneComment(t, admin, live), "/"+roleA))

	ordinary, err := f.Clone(ctx, plan)
	require.NoError(t, err)
	cleanupDatabase(t, admin, ordinary.Name())
	copied, err := f.Clone(ctx, plan)
	require.NoError(t, err)
	cleanupDatabase(t, admin, copied.Name())
	_, err = admin.ExecContext(ctx, "COMMENT ON DATABASE "+pq.QuoteIdentifier(copied.Name())+" IS "+pq.QuoteLiteral(cloneComment(t, admin, orphan)))
	require.NoError(t, err)

	open := &installer{dsn: os.Getenv("TABULA_TEST_DSN")}
	other, err := open.open(ordinary.Name())
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	foreign := postgresfixture.Fixture{Admin: other}
	visitor, err := open.open(orphan)
	require.NoError(t, err)
	t.Cleanup(func() { _ = visitor.Close() })
	_, err = visitor.ExecContext(ctx, "SELECT 1") // An unrelated login can also keep a clone alive.
	require.NoError(t, err)
	require.NoError(t, b.cmd.Process.Kill())
	require.Error(t, b.cmd.Wait())
	_, err = admin.ExecContext(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1,0))", roleB)
	require.NoError(t, err)
	// Collection works even if the original login role has already gone.
	roles, err := f.PruneLoginRoles(ctx, 0)
	require.NoError(t, err)
	require.Contains(t, roles, roleB)
	removed, err := f.PruneClones(ctx, 0)
	require.NoError(t, err)
	require.Empty(t, removed)
	require.NoError(t, visitor.PingContext(ctx))
	require.NoError(t, visitor.Close())
	waitForNoClients(t, admin, orphan)
	removed, err = foreign.PruneClones(ctx, 0)
	require.NoError(t, err)
	require.Empty(t, removed)

	type result struct {
		removed []string
		err     error
	}
	start, done := make(chan struct{}), make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			names, err := f.PruneClones(ctx, 0)
			done <- result{names, err}
		}()
	}
	close(start)
	for range 2 {
		r := <-done
		require.NoError(t, r.err)
		removed = append(removed, r.removed...)
	}
	require.Equal(t, []string{orphan}, removed)
	require.False(t, cloneExists(t, admin, orphan))
	for _, name := range []string{live, ordinary.Name(), copied.Name()} {
		require.True(t, cloneExists(t, admin, name))
	}
	a.command(t, "release", "released")
	require.NoError(t, a.cmd.Wait())
	// The combined sweep uses the configured grace period, timed on the server.
	base := cloneComment(t, admin, live)
	removed, err = f.Prune(ctx, time.Hour, plan)
	require.NoError(t, err)
	require.NotContains(t, removed, live)
	require.True(t, strings.HasPrefix(cloneComment(t, admin, live), base+" idle "))
	reconnected, err := open.open(live)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reconnected.Close() })
	require.NoError(t, reconnected.PingContext(ctx))
	removed, err = f.PruneClones(ctx, 0)
	require.NoError(t, err)
	require.Empty(t, removed)
	require.Equal(t, base, cloneComment(t, admin, live))
	require.NoError(t, reconnected.Close())
	waitForNoClients(t, admin, live)
	_, err = admin.ExecContext(ctx, "COMMENT ON DATABASE "+pq.QuoteIdentifier(live)+" IS "+pq.QuoteLiteral(base+" idle 2000-01-01T00:00:00Z"))
	require.NoError(t, err)
	removed, err = f.Prune(ctx, time.Hour, plan)
	require.NoError(t, err)
	require.Contains(t, removed, live)
}

func TestLeasedCloneDropAndOwnership(t *testing.T) {
	admin, f := loginFixture(t)
	ctx := t.Context()
	r, err := f.AcquireLoginRole(ctx, postgresfixture.LoginRoleOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Release(context.Background()) })
	plan := postgresfixture.Installation{Revision: t.TempDir(), Base: []byte("SELECT 1")}
	template, err := plan.TemplateName()
	require.NoError(t, err)
	cleanupDatabase(t, admin, template)
	clone, err := f.CloneWithLease(ctx, plan, r)
	require.NoError(t, err)
	cleanupDatabase(t, admin, clone.Name())
	other, err := (&installer{dsn: os.Getenv("TABULA_TEST_DSN")}).open(clone.Name())
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	foreign := postgresfixture.Fixture{Admin: other, Backend: f.Backend}
	_, err = foreign.CloneWithLease(ctx, plan, r)
	require.ErrorContains(t, err, "maintenance database")
	require.NoError(t, other.Close())

	collectible, err := f.CloneWithLease(ctx, plan, r)
	require.NoError(t, err)
	cleanupDatabase(t, admin, collectible.Name())
	oldMark := cloneComment(t, admin, clone.Name())
	require.NoError(t, clone.Drop(ctx))
	require.NoError(t, clone.Drop(ctx))
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(clone.Name())+" TEMPLATE template0")
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, "COMMENT ON DATABASE "+pq.QuoteIdentifier(clone.Name())+" IS "+pq.QuoteLiteral(oldMark))
	require.NoError(t, err)
	require.ErrorContains(t, clone.Drop(ctx), "ownership mark")
	require.NoError(t, r.Release(ctx))
	removed, err := f.PruneClones(ctx, 0)
	require.NoError(t, err)
	require.Contains(t, removed, collectible.Name())
	require.NoError(t, collectible.Drop(ctx))
	require.NotContains(t, removed, clone.Name())
	require.True(t, cloneExists(t, admin, clone.Name()))
	_, err = f.CloneWithLease(ctx, plan, r)
	require.ErrorContains(t, err, "lease has ended")
}

type releaseDuringCopy struct {
	postgresfixture.Backend
	login *postgresfixture.LoginRole
	name  string
}

func (b *releaseDuringCopy) Clone(ctx context.Context, conn *sql.Conn, from, to string) error {
	if err := b.Backend.Clone(ctx, conn, from, to); err != nil {
		return err
	}
	if strings.HasPrefix(to, "tabula_test_") {
		b.name = to
		return b.login.Release(ctx)
	}
	return nil
}

func TestLeaseEndsDuringClone(t *testing.T) {
	admin, f := loginFixture(t)
	ctx := t.Context()
	r, err := f.AcquireLoginRole(ctx, postgresfixture.LoginRoleOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Release(context.Background()) })
	plan := postgresfixture.Installation{Revision: t.TempDir(), Base: []byte("SELECT 1")}
	template, err := plan.TemplateName()
	require.NoError(t, err)
	cleanupDatabase(t, admin, template)
	b := &releaseDuringCopy{Backend: f.Backend, login: r}
	f.Backend = b
	db, err := f.CloneWithLease(ctx, plan, r)
	require.ErrorContains(t, err, "lease has ended")
	require.Empty(t, db.Name())
	require.NotEmpty(t, b.name)
	require.False(t, cloneExists(t, admin, b.name))
}
