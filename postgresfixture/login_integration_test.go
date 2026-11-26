//go:build integration

package postgresfixture_test

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/0xJean/tabula/postgresfixture"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const loginWorkerEnv = "TABULA_FIXTURE_LOGIN_WORKER"
const workerLabel = "worktree_0123456789abcdef"

func loginClient(t *testing.T, database string, login *postgresfixture.LoginRole) *sql.DB {
	t.Helper()
	cfg, err := pq.NewConfig(os.Getenv("TABULA_TEST_DSN"))
	require.NoError(t, err)
	cfg.Database, cfg.User, cfg.Password = database, login.Name(), login.Password()
	connector, err := pq.NewConnectorConfig(cfg)
	require.NoError(t, err)
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func loginFixture(t *testing.T) (*sql.DB, postgresfixture.Fixture) {
	t.Helper()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	return admin, postgresfixture.Fixture{Admin: admin, Backend: &installer{dsn: dsn}}
}

// Only name and protocol acknowledgements cross the process pipe. Each worker
// authenticates with its own in-memory secret and keeps its lease between tests.
func TestLoginWorker(t *testing.T) {
	member, helper := os.LookupEnv(loginWorkerEnv)
	if !helper {
		return
	}
	_, f := loginFixture(t)
	ctx := t.Context()
	role, err := f.AcquireLoginRole(ctx, postgresfixture.LoginRoleOptions{MemberOf: []string{member}, ConnectionLimit: new(3), Label: workerLabel})
	require.NoError(t, err)
	fmt.Println(role.Name())
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		command := scanner.Text()
		if command == "clone" {
			// A suite may reserve a small pool for leases and a separate pool for copies.
			_, clones := loginFixture(t)
			db, err := clones.CloneWithLease(ctx, postgresfixture.Installation{Revision: member, Base: []byte("SELECT 1")}, role)
			require.NoError(t, err)
			fmt.Println(db.Name())
			continue
		}
		if command == "release" {
			require.NoError(t, role.Release(ctx))
			require.NoError(t, role.Release(ctx))
			fmt.Println("released")
			return
		}
		method, database, ok := strings.Cut(command, " ")
		require.True(t, ok)
		require.Contains(t, []string{"connect", "hold"}, method)
		db := loginClient(t, database, role)
		conn, err := db.Conn(ctx)
		require.NoError(t, err)
		if method == "hold" {
			_, err = conn.ExecContext(ctx, "SELECT pg_catalog.pg_advisory_lock(pg_catalog.hashtextextended($1,0))", "client:"+role.Name())
			require.NoError(t, err)
			fmt.Println("connected")
			require.True(t, scanner.Scan())
			require.Equal(t, "resume", scanner.Text())
		}
		var user string
		err = conn.QueryRowContext(ctx, "SELECT current_user").Scan(&user)
		require.NoError(t, conn.Close())
		require.NoError(t, db.Close())
		require.NoError(t, err)
		require.Equal(t, role.Name(), user)
		fmt.Println("disconnected")
	}
	require.NoError(t, scanner.Err())
	t.Fatal("test controller disconnected")
}

type loginWorker struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Scanner
}

func startLoginWorker(t *testing.T, member string) *loginWorker {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLoginWorker$")
	cmd.Env = append(os.Environ(), loginWorkerEnv+"="+member)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return &loginWorker{cmd: cmd, input: stdin, output: bufio.NewScanner(stdout)}
}

func (w *loginWorker) read(t *testing.T) string {
	t.Helper()
	require.True(t, w.output.Scan(), "test worker exited before acknowledging command")
	return w.output.Text()
}
func (w *loginWorker) command(t *testing.T, command, want string) {
	t.Helper()
	_, err := fmt.Fprintln(w.input, command)
	require.NoError(t, err)
	require.Equal(t, want, w.read(t))
}

func roleExists(t *testing.T, db *sql.DB, role string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT FROM pg_catalog.pg_roles WHERE rolname=$1)", role).Scan(&exists))
	return exists
}

func TestProcessLoginLifetime(t *testing.T) {
	admin, f := loginFixture(t)
	ctx := t.Context()
	member := fmt.Sprintf("tabula_member_%d", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, "CREATE ROLE "+member)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = f.PruneLoginRoles(context.Background(), 0); _, _ = admin.Exec("DROP ROLE " + member) })
	plan := postgresfixture.Installation{Revision: t.TempDir(), Base: []byte("SELECT 1")}
	clone, err := f.Clone(ctx, plan)
	require.NoError(t, err)
	template, err := plan.TemplateName()
	require.NoError(t, err)
	var survivor postgresfixture.Database
	t.Cleanup(func() {
		if survivor.Name() != "" {
			require.NoError(t, survivor.Drop(context.Background()))
		}
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + pq.QuoteIdentifier(clone.Name()))
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + pq.QuoteIdentifier(template))
	})
	_, err = admin.ExecContext(ctx, "REVOKE ALL ON DATABASE "+pq.QuoteIdentifier(clone.Name())+" FROM PUBLIC")
	require.NoError(t, err)

	first, second := startLoginWorker(t, member), startLoginWorker(t, member)
	a, b := first.read(t), second.read(t)
	require.Regexp(t, "^tabula_login_[a-f0-9]{24}_"+workerLabel+"$", a)
	require.Regexp(t, "^tabula_login_[a-f0-9]{24}_"+workerLabel+"$", b)
	require.Len(t, a, 63)
	require.Len(t, b, 63)
	require.NotEqual(t, a, b)
	for _, name := range []string{a, b} {
		var safe bool
		require.NoError(t, admin.QueryRowContext(ctx, `SELECT r.rolcanlogin AND NOT (r.rolinherit OR r.rolsuper
			OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls) AND r.rolconnlimit=3
			AND NOT pg_catalog.pg_has_role(r.oid,$2::text,'USAGE')
			AND ARRAY(SELECT p.rolname::text FROM pg_catalog.pg_auth_members m
				JOIN pg_catalog.pg_roles p ON p.oid=m.roleid WHERE m.member=r.oid)=ARRAY[$2::text]
			FROM pg_catalog.pg_roles r WHERE r.rolname=$1`, name, member).Scan(&safe))
		require.True(t, safe)
		_, err = admin.ExecContext(ctx, "GRANT CONNECT ON DATABASE "+pq.QuoteIdentifier(clone.Name())+" TO "+pq.QuoteIdentifier(name))
		require.NoError(t, err)
	}
	first.command(t, "connect "+clone.Name(), "disconnected")
	second.command(t, "connect "+clone.Name(), "disconnected")

	// Advisory locks are database-local while login roles are cluster-wide.
	other, err := (&installer{dsn: os.Getenv("TABULA_TEST_DSN")}).open(clone.Name())
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	foreign := postgresfixture.Fixture{Admin: other}
	names, err := foreign.PruneLoginRoles(ctx, 0)
	require.NoError(t, err)
	require.NotContains(t, names, a)
	require.NotContains(t, names, b)
	require.True(t, roleExists(t, admin, a))
	require.True(t, roleExists(t, admin, b))
	names, err = f.PruneLoginRoles(ctx, 0)
	require.NoError(t, err)
	require.NotContains(t, names, a)
	require.NotContains(t, names, b)
	// Both leases stay live between test cases even after the last clone is gone.
	require.NoError(t, other.Close())
	require.NoError(t, clone.Drop(ctx))
	_, err = f.Prune(ctx, time.Hour, plan)
	require.NoError(t, err)
	require.True(t, roleExists(t, admin, a))
	require.True(t, roleExists(t, admin, b))
	survivor, err = f.Clone(ctx, plan)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, "REVOKE ALL ON DATABASE "+pq.QuoteIdentifier(survivor.Name())+" FROM PUBLIC")
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, "GRANT CONNECT ON DATABASE "+pq.QuoteIdentifier(survivor.Name())+" TO "+pq.QuoteIdentifier(a))
	require.NoError(t, err)
	first.command(t, "hold "+survivor.Name(), "connected")
	require.NoError(t, second.cmd.Process.Kill())
	require.Error(t, second.cmd.Wait())
	// Synchronize with PostgreSQL noticing the dead worker's socket. This
	// transaction takes and releases the lease key before the pruners race.
	_, err = admin.ExecContext(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1,0))", b)
	require.NoError(t, err)
	type result struct {
		names []string
		err   error
	}
	done := make(chan result, 2)
	for range 2 {
		go func() { names, err := f.PruneLoginRoles(ctx, 0); done <- result{names, err} }()
	}
	var removed []string
	for range 2 {
		r := <-done
		require.NoError(t, r.err)
		removed = append(removed, r.names...)
	}
	require.Equal(t, []string{b}, removed)
	require.False(t, roleExists(t, admin, b))
	require.True(t, roleExists(t, admin, a))
	// Execute on the same client connection after the peer was killed and reaped.
	first.command(t, "resume", "disconnected")
	_, err = admin.ExecContext(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1,0))", "client:"+a)
	require.NoError(t, err)
	first.command(t, "release", "released")
	require.NoError(t, first.cmd.Wait())
	require.False(t, roleExists(t, admin, a))
}

func TestLoginReleaseRetriesWithoutDroppingObjects(t *testing.T) {
	admin, f := loginFixture(t)
	ctx := t.Context()
	name := fmt.Sprintf("tabula_role_item_%d", time.Now().UnixNano())
	execSQL := func(query string) {
		t.Helper()
		_, err := admin.ExecContext(ctx, query)
		require.NoError(t, err, query)
	}
	execSQL("CREATE TABLE " + name + " (id int)")
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP TABLE IF EXISTS " + name)
		_, _ = f.PruneLoginRoles(context.Background(), 0)
	})
	r, err := f.AcquireLoginRole(ctx, postgresfixture.LoginRoleOptions{Inherit: true})
	require.NoError(t, err)
	password := r.Password()
	require.NotEmpty(t, password)
	for _, format := range []string{"%v", "%+v", "%#v"} {
		require.NotContains(t, fmt.Sprintf(format, r), password)
	}
	execSQL("ALTER TABLE " + name + " OWNER TO " + r.Name())
	var database string
	require.NoError(t, admin.QueryRowContext(ctx, "SELECT current_database()").Scan(&database))
	execSQL("GRANT CONNECT ON DATABASE " + pq.QuoteIdentifier(database) + " TO " + r.Name())
	err = r.Release(ctx)
	require.Error(t, err)
	pgErr, ok := errors.AsType[*pq.Error](err)
	require.True(t, ok)
	require.Equal(t, "2BP01", string(pgErr.Code))
	require.Empty(t, r.Password())
	var intact bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT FROM pg_catalog.pg_database d
		CROSS JOIN LATERAL pg_catalog.aclexplode(d.datacl) a JOIN pg_catalog.pg_roles r ON r.oid=a.grantee
		WHERE d.datname=pg_catalog.current_database() AND a.privilege_type='CONNECT' AND r.rolname=$1)`, r.Name()).Scan(&intact))
	require.True(t, intact)
	execSQL("DROP TABLE " + name)
	require.NoError(t, r.Release(ctx))
	require.NoError(t, r.Release(ctx))
	require.False(t, roleExists(t, admin, r.Name()))
}

func TestFixturePruneReapsLoginAndKeepsUnmarkedRole(t *testing.T) {
	admin, f := loginFixture(t)
	ctx := t.Context()
	lookalike := fmt.Sprintf("tabula_login_%024x_labeled", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, "CREATE ROLE "+lookalike+" LOGIN")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec("DROP ROLE " + lookalike) })

	r, err := f.AcquireLoginRole(ctx, postgresfixture.LoginRoleOptions{})
	require.NoError(t, err)
	require.Regexp(t, "^tabula_login_[a-f0-9]{24}$", r.Name())
	t.Cleanup(func() { _ = r.Release(context.Background()) })
	var database string
	require.NoError(t, admin.QueryRowContext(ctx, "SELECT current_database()").Scan(&database))
	_, err = admin.ExecContext(ctx, "GRANT CONNECT ON DATABASE "+pq.QuoteIdentifier(database)+" TO "+r.Name())
	require.NoError(t, err)
	client := loginClient(t, database, r)
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, r.Release(stopped), context.Canceled)
	require.True(t, roleExists(t, admin, r.Name()))
	readMark := func() string {
		t.Helper()
		var mark string
		require.NoError(t, admin.QueryRowContext(ctx, "SELECT pg_catalog.shobj_description(oid,'pg_authid') FROM pg_catalog.pg_roles WHERE rolname=$1", r.Name()).Scan(&mark))
		return mark
	}
	base := readMark()
	_, err = f.Prune(ctx, time.Hour)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(readMark(), base+" idle "))
	_, err = f.Prune(ctx, time.Hour)
	require.NoError(t, err)
	require.True(t, roleExists(t, admin, r.Name()))

	// An observed client clears the clock, even when an immediate sweep is requested.
	clientKey := "client:" + r.Name()
	_, err = client.ExecContext(ctx, "SELECT pg_catalog.pg_advisory_lock(pg_catalog.hashtextextended($1,0))", clientKey)
	require.NoError(t, err)
	_, err = f.PruneLoginRoles(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, base, readMark())
	require.NoError(t, client.Close())
	_, err = admin.ExecContext(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1,0))", clientKey)
	require.NoError(t, err)

	_, err = f.Prune(ctx, time.Hour)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(readMark(), base+" idle "))
	// Backdate this fixture's mark to exercise expiry without an hour-long test.
	_, err = admin.ExecContext(ctx, "COMMENT ON ROLE "+pq.QuoteIdentifier(r.Name())+" IS "+pq.QuoteLiteral(base+" idle 2000-01-01T00:00:00Z"))
	require.NoError(t, err)
	databases, err := f.Prune(ctx, time.Hour)
	require.NoError(t, err)
	require.NotContains(t, databases, r.Name())
	require.False(t, roleExists(t, admin, r.Name()))
	require.True(t, roleExists(t, admin, lookalike))
	require.NoError(t, r.Release(ctx))
}
