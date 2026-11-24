//go:build integration

package pgxdriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/stdsql"
)

type readSession interface {
	tabula.Session[string]
	ReadOnly() bool
}
type txBody func(context.Context, readSession) error
type sharedBody func(ctx context.Context, scoped, reader, alias readSession) error

// Run the same transaction and cursor assertions through each driver's public entry points.
type sharedAdapter struct {
	within    func(context.Context, txBody) error
	shared    func(context.Context, sharedBody) error
	savepoint func(context.Context, readSession, txBody) error
}

func TestSharedReadRoleAndCursorLifetime(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	suffix := fmt.Sprint(time.Now().UnixNano())
	writer, reader := "shared_writer_"+suffix, "shared_reader_"+suffix
	schema := "shared_" + suffix
	table := schema + ".items"
	exec := func(q string) { t.Helper(); _, err := db.ExecContext(ctx, q); require.NoError(t, err) }
	for _, r := range []string{writer, reader} {
		exec("CREATE ROLE " + r + " NOLOGIN")
		t.Cleanup(func() { _, _ = db.Exec("DROP ROLE " + r) })
	}
	exec("CREATE SCHEMA " + schema)
	t.Cleanup(func() { _, _ = db.Exec("DROP SCHEMA " + schema + " CASCADE") })
	exec("CREATE TABLE " + table + "(id integer PRIMARY KEY)")
	exec("GRANT USAGE ON SCHEMA " + schema + " TO " + writer + ", " + reader)
	exec("GRANT SELECT, INSERT ON " + table + " TO " + writer)
	exec("GRANT SELECT ON " + table + " TO " + reader)

	policy := stdsql.ScopePolicy[string]{ManagedVars: []string{"app.scope"},
		ScopeVars:  func(s string) map[string]string { return map[string]string{"app.scope": s} },
		IsUnscoped: func(s string) bool { return s == "all" }}
	trace := new(pgxTraceRec)
	std, err := stdsql.NewRunner(db, policy, stdsql.RunnerOptions{Role: writer, Unscoped: stdsql.UnscopedReadOnly, UnscopedReadRole: reader, Tracer: trace})
	require.NoError(t, err)
	pg, err := pgxdriver.NewRunner(pool, pgxdriver.ScopePolicy[string](policy), pgxdriver.RunnerOptions{Role: writer, Unscoped: pgxdriver.UnscopedReadOnly, UnscopedReadRole: reader, Tracer: trace})
	require.NoError(t, err)
	adapters := map[string]sharedAdapter{
		"stdsql": {
			within: func(ctx context.Context, fn txBody) error {
				return std.Within(ctx, "a", func(ctx context.Context, s *stdsql.Session[string]) error { return fn(ctx, s) })
			},
			shared: func(ctx context.Context, fn sharedBody) error {
				return std.WithinShared(ctx, func(ctx context.Context, sh *stdsql.Shared[string]) error {
					return fn(ctx, sh.Scoped("a"), sh.Unscoped(), sh.Scoped("all"))
				})
			},
			savepoint: func(ctx context.Context, s readSession, fn txBody) error {
				return s.(*stdsql.Session[string]).Savepoint(ctx, "nested", func(ctx context.Context) error { return fn(ctx, s) })
			},
		},
		"pgx": {
			within: func(ctx context.Context, fn txBody) error {
				return pg.Within(ctx, "a", func(ctx context.Context, s *pgxdriver.Session[string]) error { return fn(ctx, s) })
			},
			shared: func(ctx context.Context, fn sharedBody) error {
				return pg.WithinShared(ctx, func(ctx context.Context, sh *pgxdriver.Shared[string]) error {
					return fn(ctx, sh.Scoped("a"), sh.Unscoped(), sh.Scoped("all"))
				})
			},
			savepoint: func(ctx context.Context, s readSession, fn txBody) error {
				return s.(*pgxdriver.Session[string]).Savepoint(ctx, func(ctx context.Context, child *pgxdriver.Session[string]) error { return fn(ctx, child) })
			},
		},
	}
	type item struct {
		ID int `db:"id,key"`
	}
	items := tabula.Define(tabula.NewRegistry(tabula.RegistryConfig[string]{}), tabula.TableSpec[item, int]{Name: table})
	for name, a := range adapters {
		t.Run(name, func(t *testing.T) {
			marker := errors.New("rollback")
			start := len(trace.snapshot())
			err := a.shared(ctx, func(ctx context.Context, scoped, read, alias readSession) error {
				require.False(t, scoped.ReadOnly())
				_, err := scoped.Exec(ctx, "INSERT INTO "+table+" VALUES (1)")
				require.NoError(t, err)
				for _, s := range []readSession{read, alias} {
					require.True(t, s.ReadOnly())
					require.ErrorIs(t, items.Bind(s).Insert(ctx, &item{ID: 2}), tabula.ErrReadOnly)
					if native, ok := s.(*pgxdriver.Session[string]); ok {
						_, err := pgxdriver.CopyFrom(ctx, native, items, []item{{ID: 2}})
						require.ErrorIs(t, err, tabula.ErrReadOnly)
					}
					var role, scope string
					var n int
					var mayWrite bool
					readCtx := tabula.WithStatementMeta(ctx, tabula.StatementMeta{Key: "shared.read"})
					require.NoError(t, s.Scan(readCtx, "SELECT current_user, current_setting('app.scope'), count(*), has_table_privilege(current_user, $1, 'INSERT') FROM "+table, []any{table}, &role, &scope, &n, &mayWrite))
					require.Equal(t, reader, role)
					require.Empty(t, scope)
					require.Equal(t, 1, n)
					require.False(t, mayWrite)
					_, err = s.Exec(ctx, "SELECT 1")
					require.ErrorIs(t, err, tabula.ErrReadOnly)
					require.ErrorIs(t, s.Returning(ctx, "INSERT INTO "+table+" VALUES (2) RETURNING id", nil, &n), tabula.ErrReadOnly)
					require.ErrorIs(t, s.Scan(ctx, "UPDATE "+table+" SET id=2 RETURNING id", nil, &n), tabula.ErrReadOnly)
					require.NoError(t, s.Each(ctx, "SELECT id FROM "+table, nil, func(scan func(...any) error) error { return scan(&n) }))
				}
				require.NoError(t, a.savepoint(ctx, read, func(ctx context.Context, child readSession) error {
					require.True(t, child.ReadOnly())
					var role string
					if err := child.Scan(ctx, "SELECT current_user", nil, &role); err != nil {
						return err
					}
					require.Equal(t, reader, role)
					return nil
				}))
				var role, scope string
				writeCtx := tabula.WithStatementMeta(ctx, tabula.StatementMeta{Key: "shared.write"})
				require.NoError(t, scoped.Scan(writeCtx, "SELECT current_user, current_setting('app.scope')", nil, &role, &scope))
				require.Equal(t, writer, role)
				require.Equal(t, "a", scope)
				_, err = scoped.Exec(ctx, "INSERT INTO "+table+" VALUES (2)")
				require.NoError(t, err)
				return marker
			})
			require.ErrorIs(t, err, marker)
			var n int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n))
			require.Zero(t, n)
			roles := map[string]string{}
			for _, e := range trace.snapshot()[start:] {
				roles[e.info.Key] = e.info.Role
			}
			require.Equal(t, reader, roles["shared.read"])
			require.Equal(t, writer, roles["shared.write"])
			for _, end := range []string{"canceled", "deadline"} {
				t.Run(end, func(t *testing.T) { checkPullBoundary(t, a, end, false) })
			}
			for _, boundary := range []string{"transaction", "shared", "savepoint"} {
				for _, rollback := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/rollback=%t", boundary, rollback), func(t *testing.T) {
						checkPullBoundary(t, a, boundary, rollback)
					})
				}
			}
		})
	}
}

func checkPullBoundary(t *testing.T, a sharedAdapter, boundary string, rollback bool) {
	t.Helper()
	type row struct {
		N int `db:"n"`
	}
	view := tabula.DefineView(tabula.NewRegistry(tabula.RegistryConfig[string]{}), tabula.ViewSpec[row]{Name: "series"})
	var next func() (row, error, bool)
	result := make(chan error, 1)
	var want error
	if rollback {
		want = errors.New("rollback cursor")
	}
	deadline, clearDeadline := context.WithTimeout(t.Context(), 5*time.Second)
	defer clearDeadline()
	var ctx context.Context
	var cancel context.CancelFunc
	if boundary == "deadline" {
		ctx, cancel = context.WithTimeout(deadline, time.Second)
		want = context.DeadlineExceeded
	} else {
		ctx, cancel = context.WithCancel(deadline)
		if boundary == "canceled" {
			want = context.Canceled
		}
	}
	defer cancel()
	body := func(ctx context.Context, s readSession) error {
		var stop func()
		next, stop = iter.Pull2(view.Bind(s).Iter(ctx, "SELECT generate_series(1,3) AS n"))
		first, err, ok := next()
		if err != nil || !ok || first.N != 1 {
			stop()
			return fmt.Errorf("first row %+v, ok %v, err %v", first, ok, err)
		}
		t.Cleanup(stop)
		if boundary == "canceled" {
			cancel()
			return nil
		}
		if boundary == "deadline" {
			<-ctx.Done()
			return ctx.Err()
		}
		return want
	}
	go func() {
		switch boundary {
		case "savepoint":
			result <- a.within(ctx, func(ctx context.Context, s readSession) error {
				err := a.savepoint(ctx, s, body)
				if !errors.Is(err, want) {
					return fmt.Errorf("savepoint result: %w", err)
				}
				var n int
				if err := s.Scan(ctx, "SELECT 1", nil, &n); err != nil {
					return err
				}
				return want
			})
		case "shared":
			result <- a.shared(ctx, func(ctx context.Context, _, s, _ readSession) error { return body(ctx, s) })
		default:
			result <- a.within(ctx, body)
		}
	}()
	select {
	case err := <-result:
		require.ErrorIs(t, err, want)
	case <-deadline.Done():
		t.Fatal("cleanup waited for the iterator consumer")
	}
	_, err, ok := next()
	require.True(t, ok)
	if ctx.Err() != nil {
		require.ErrorIs(t, err, tabula.ErrCanceled)
		require.ErrorIs(t, err, ctx.Err())
	} else {
		require.ErrorIs(t, err, tabula.ErrSessionClosed)
	}
	_, _, ok = next()
	require.False(t, ok)
}
