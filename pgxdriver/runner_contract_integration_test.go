//go:build integration

package pgxdriver_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/stdsql"
)

type sessionCallback func(context.Context, tabula.Session[struct{}]) error

func onSession[S tabula.Session[struct{}]](fn sessionCallback) func(context.Context, S) error {
	return func(ctx context.Context, s S) error { return fn(ctx, s) }
}

// This test connection drops one server reply after the final successful
// statement. A separate, ordinary connection verifies what PostgreSQL committed.
type dropReplyConn struct {
	net.Conn
	drop *atomic.Bool
}

func (c dropReplyConn) Read(buf []byte) (int, error) {
	n, err := c.Conn.Read(buf)
	if n > 0 && c.drop.Swap(false) {
		_ = c.Close()
		return 0, io.ErrUnexpectedEOF
	}
	return n, err
}

func TestRunnerStatementAndCommitContract(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	table := fmt.Sprintf("tabula_runner_contract_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE TABLE "+table+" (id integer UNIQUE DEFERRABLE INITIALLY DEFERRED)")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP TABLE "+table) })

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.MaxConns = 1
	var drop atomic.Bool
	dial := cfg.ConnConfig.DialFunc
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return dropReplyConn{conn, &drop}, nil
	}
	db := stdlib.OpenDB(*cfg.ConnConfig)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	policy := tabula.StatementPolicy{Read: []string{"select", "with"}, Write: []string{"insert", "update", "delete"}, RequireReturning: true}
	single, err := pgxdriver.NewRunner(pool, pgxdriver.NoScopePolicy(), pgxdriver.RunnerOptions{SingleStatement: true})
	require.NoError(t, err)
	require.NoError(t, single.Within(ctx, struct{}{}, func(ctx context.Context, s *pgxdriver.Session[struct{}]) error {
		_, err := s.Exec(ctx, "SELECT 1; SELECT 2")
		require.ErrorIs(t, err, tabula.ErrForbiddenStatement)
		_, err = s.Exec(ctx, "SELECT 1")
		return err
	}))
	standard, err := stdsql.NewRunner(db, stdsql.NoScopePolicy(), stdsql.RunnerOptions{Statements: policy, ExposeTx: true, Unscoped: stdsql.UnscopedReadWrite})
	require.NoError(t, err)
	native, err := pgxdriver.NewRunner(pool, pgxdriver.NoScopePolicy(), pgxdriver.RunnerOptions{Statements: policy, ExposeTx: true, Unscoped: pgxdriver.UnscopedReadWrite})
	require.NoError(t, err)
	scope := struct{}{}
	stdBody := func(context.Context, *stdsql.Session[struct{}]) error { return nil }
	pgxBody := func(context.Context, *pgxdriver.Session[struct{}]) error { return nil }
	type path struct {
		name string
		run  func(context.Context, sessionCallback) error
	}
	transactions := []path{
		{"stdsql/within", func(ctx context.Context, fn sessionCallback) error {
			return standard.Within(ctx, scope, onSession[*stdsql.Session[struct{}]](fn))
		}},
		{"stdsql/shared", func(ctx context.Context, fn sessionCallback) error {
			return standard.WithinShared(ctx, func(ctx context.Context, sh *stdsql.Shared[struct{}]) error { return fn(ctx, sh.Unscoped()) })
		}},
		{"pgx/within", func(ctx context.Context, fn sessionCallback) error {
			return native.Within(ctx, scope, onSession[*pgxdriver.Session[struct{}]](fn))
		}},
		{"pgx/shared", func(ctx context.Context, fn sessionCallback) error {
			return native.WithinShared(ctx, func(ctx context.Context, sh *pgxdriver.Shared[struct{}]) error { return fn(ctx, sh.Scoped(scope)) })
		}},
	}
	otherHandles := []path{
		{"stdsql/raw", func(ctx context.Context, fn sessionCallback) error {
			return standard.WithinRaw(ctx, scope, onSession[*stdsql.RawSession[struct{}]](fn))
		}},
		{"stdsql/before_commit", func(ctx context.Context, fn sessionCallback) error {
			return standard.WithinWith(ctx, scope, stdsql.RunOptions[struct{}]{BeforeCommit: onSession[*stdsql.Session[struct{}]](fn)}, stdBody)
		}},
		{"pgx/raw", func(ctx context.Context, fn sessionCallback) error {
			return native.WithinRaw(ctx, scope, onSession[*pgxdriver.RawSession[struct{}]](fn))
		}},
		{"pgx/before_commit", func(ctx context.Context, fn sessionCallback) error {
			return native.WithinWith(ctx, scope, pgxdriver.RunOptions[struct{}]{BeforeCommit: onSession[*pgxdriver.Session[struct{}]](fn)}, pgxBody)
		}},
	}
	savepoints := []path{
		{"stdsql/savepoint", func(ctx context.Context, fn sessionCallback) error {
			return standard.Within(ctx, scope, func(ctx context.Context, s *stdsql.Session[struct{}]) error {
				return s.Savepoint(ctx, "checked", func(ctx context.Context) error { return fn(ctx, s) })
			})
		}},
		{"pgx/savepoint", func(ctx context.Context, fn sessionCallback) error {
			return native.Within(ctx, scope, func(ctx context.Context, s *pgxdriver.Session[struct{}]) error {
				return s.Savepoint(ctx, onSession[*pgxdriver.Session[struct{}]](fn))
			})
		}},
	}
	insert := "INSERT INTO " + table + " (id) VALUES ($1)"
	for _, p := range savepoints {
		t.Run(p.name+"/release_error", func(t *testing.T) {
			err := p.run(ctx, func(ctx context.Context, s tabula.Session[struct{}]) error {
				err := s.Scan(ctx, "SELECT 1 / 0", nil, new(int))
				require.Equal(t, "22012", tabula.SQLState(err))
				return nil // Ask the driver to release an aborted savepoint.
			})
			require.Equal(t, "25P02", tabula.SQLState(err))
			require.NotErrorIs(t, err, tabula.ErrCommit)
		})
	}
	for _, p := range slices.Concat(transactions, otherHandles, savepoints) {
		t.Run(p.name+"/methods", func(t *testing.T) {
			err := p.run(ctx, func(ctx context.Context, s tabula.Session[struct{}]) error {
				var id int
				_, err := s.Exec(ctx, "SELECT 1")
				require.ErrorIs(t, err, tabula.ErrForbiddenStatement)
				require.ErrorIs(t, s.Each(ctx, "WITH added AS ("+insert+" RETURNING id) SELECT id FROM added", []any{21}, nil), tabula.ErrForbiddenStatement)
				require.ErrorIs(t, s.Scan(ctx, "SELECT 1; SELECT 2", nil, &id), tabula.ErrForbiddenStatement)
				require.ErrorIs(t, s.Returning(ctx, insert, []any{21}, &id), tabula.ErrForbiddenStatement)
				_, err = s.Exec(ctx, insert, 21)
				require.NoError(t, err)
				require.NoError(t, s.Returning(ctx, "UPDATE "+table+" SET id = 22 WHERE id = 21 RETURNING id", nil, &id))
				require.Equal(t, 22, id)
				err = s.Each(ctx, "SELECT id FROM "+table+" WHERE id = 22", nil, func(scan func(...any) error) error { return scan(&id) })
				require.NoError(t, err)
				_, err = s.Exec(ctx, "DELETE FROM "+table+" WHERE id = 22")
				return err
			})
			require.NoError(t, err)
			cause := errors.New("step stopped")
			err = p.run(ctx, func(context.Context, tabula.Session[struct{}]) error { return cause })
			require.ErrorIs(t, err, cause)
			require.NotErrorIs(t, err, tabula.ErrCommit)
		})
	}
	for i, p := range transactions {
		t.Run(p.name+"/commit", func(t *testing.T) {
			err := p.run(ctx, func(ctx context.Context, s tabula.Session[struct{}]) error {
				_, err := s.Exec(ctx, "INSERT INTO "+table+" (id) VALUES (31), (31)")
				return err
			})
			require.ErrorIs(t, err, tabula.ErrCommit)
			require.Equal(t, "23505", tabula.SQLState(err))
			require.False(t, tabula.IsRetryable(err))
			err = p.run(ctx, func(ctx context.Context, s tabula.Session[struct{}]) error {
				return s.Scan(ctx, "SELECT 1 / 0", nil, new(int))
			})
			require.Equal(t, "22012", tabula.SQLState(err))
			require.NotErrorIs(t, err, tabula.ErrCommit)
			cancelCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			err = p.run(cancelCtx, func(context.Context, tabula.Session[struct{}]) error { cancel(); return nil })
			require.ErrorIs(t, err, context.Canceled)
			require.NotErrorIs(t, err, tabula.ErrCommit)

			id := 100 + i
			err = p.run(ctx, func(ctx context.Context, s tabula.Session[struct{}]) error {
				_, err := s.Exec(ctx, insert, id)
				if err == nil {
					drop.Store(true)
				}
				return err
			})
			require.ErrorIs(t, err, tabula.ErrCommit)
			require.False(t, drop.Load(), "the test connection discarded the reply")
			require.Empty(t, tabula.SQLState(err))
			require.False(t, tabula.IsRetryable(err))
			var count int
			require.NoError(t, admin.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE id = $1", id).Scan(&count))
			require.Equal(t, 1, count)
		})
	}
	require.NoError(t, native.Within(ctx, scope, func(ctx context.Context, s *pgxdriver.Session[struct{}]) error {
		err := s.Scan(ctx, "SELECT @n", []any{pgx.NamedArgs{"n": 1}}, new(int))
		require.ErrorIs(t, err, tabula.ErrForbiddenStatement)
		return s.Scan(ctx, "SELECT $1::int", []any{1}, new(int))
	}))
}
