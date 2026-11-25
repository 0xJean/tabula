//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/stdsql"
	"github.com/stretchr/testify/require"
)

type session = tabula.Session[struct{}]
type callback = func(context.Context, session) error

func adapt[S session](fn callback) func(context.Context, S) error {
	return func(ctx context.Context, s S) error { return fn(ctx, s) }
}

func checkRetry(t *testing.T, retry func(callback) error) {
	t.Helper()
	var first context.Context
	require.NoError(t, retry(func(ctx context.Context, s session) error {
		if first == nil {
			first = ctx
			return tabula.ErrSerialization
		}
		var value int
		require.ErrorIs(t, s.Scan(first, "SELECT 1", nil, &value), tabula.ErrCallbackContext)
		return s.Scan(ctx, "SELECT 1", nil, &value)
	}))
}

// Both adapters must recognize the same set of contexts on all session methods.
func exerciseCallback(t *testing.T, run func(callback, callback) error) {
	t.Helper()
	var earlier context.Context
	var closed session
	parent := t.Context()
	var result int
	require.NoError(t, run(func(ctx context.Context, s session) error {
		earlier, closed = ctx, s
		for _, other := range []context.Context{parent, tabula.MarkTransaction(parent), tabula.WithoutTransaction(ctx)} {
			_, err := s.Exec(other, "SELECT 1")
			require.ErrorIs(t, err, tabula.ErrCallbackContext)
			require.ErrorIs(t, s.Scan(other, "SELECT 1", nil, &result), tabula.ErrCallbackContext)
			require.ErrorIs(t, s.Returning(other, "SELECT 1", nil, &result), tabula.ErrCallbackContext)
			require.ErrorIs(t, s.Each(other, "SELECT 1", nil, nil), tabula.ErrCallbackContext)
		}
		derived, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		require.NoError(t, s.Scan(derived, "SELECT 1", nil, &result))
		require.Equal(t, 1, result)
		return s.Each(context.WithoutCancel(derived), "SELECT 1", nil, func(scan func(...any) error) error {
			require.ErrorIs(t, s.Scan(ctx, "SELECT 1", nil, &result), tabula.ErrSessionBusy)
			return scan(&result)
		})
	}, func(ctx context.Context, s session) error {
		require.ErrorIs(t, s.Scan(earlier, "SELECT 1", nil, &result), tabula.ErrCallbackContext)
		require.ErrorIs(t, closed.Scan(ctx, "SELECT 1", nil, &result), tabula.ErrSessionClosed)
		return s.Scan(ctx, "SELECT 1", nil, &result)
	}))
	// Open two independent transactions from the same request context.
	// Both handles are live while the inner callback runs.
	noop := func(context.Context, session) error { return nil }
	require.NoError(t, run(func(outer context.Context, a session) error {
		return run(func(inner context.Context, b session) error {
			require.ErrorIs(t, a.Scan(inner, "SELECT 1", nil, &result), tabula.ErrCallbackContext)
			require.ErrorIs(t, b.Scan(outer, "SELECT 1", nil, &result), tabula.ErrCallbackContext)
			return b.Scan(inner, "SELECT 1", nil, &result)
		}, noop)
	}, noop))
	require.ErrorIs(t, closed.Scan(parent, "SELECT 1", nil, &result), tabula.ErrSessionClosed)
	require.NoError(t, run(func(ctx context.Context, s session) error {
		require.ErrorIs(t, s.Scan(earlier, "SELECT 1", nil, &result), tabula.ErrCallbackContext)
		return s.Scan(ctx, "SELECT 1", nil, &result)
	}, func(context.Context, session) error { return nil }))
}

func TestCallbackOwnership(t *testing.T) {
	db, pool := databases(t)
	parent, scope := t.Context(), struct{}{}
	var result int
	checkShared := func(t *testing.T, ctx context.Context, s session) {
		t.Helper()
		require.ErrorIs(t, s.Scan(parent, "SELECT 1", nil, &result), tabula.ErrCallbackContext)
		require.NoError(t, s.Scan(ctx, "SELECT 1", nil, &result))
	}
	t.Run("pq", func(t *testing.T) {
		r, err := stdsql.NewRunner(db, stdsql.NoScopePolicy(), stdsql.RunnerOptions{RequireCallbackContext: true, ExposeTx: true, Unscoped: stdsql.UnscopedReadWrite})
		require.NoError(t, err)
		exerciseCallback(t, func(body, before callback) error {
			opts := stdsql.RunOptions[struct{}]{BeforeCommit: adapt[*stdsql.Session[struct{}]](before)}
			return r.WithinWith(t.Context(), scope, opts, adapt[*stdsql.Session[struct{}]](body))
		})
		checkRetry(t, func(fn callback) error {
			return r.WithinRetry(parent, scope, tabula.RetryConfig{MaxAttempts: 2, BaseDelay: time.Millisecond}, adapt[*stdsql.Session[struct{}]](fn))
		})
		require.NoError(t, r.WithinShared(parent, func(ctx context.Context, sh *stdsql.Shared[struct{}]) error {
			checkShared(t, ctx, sh.Scoped(scope))
			checkShared(t, ctx, sh.Unscoped())
			return nil
		}))
		require.NoError(t, r.WithinRaw(parent, scope, func(ctx context.Context, s *stdsql.RawSession[struct{}]) error {
			checkShared(t, ctx, s)
			require.ErrorIs(t, s.Savepoint(parent, "one", nil), tabula.ErrCallbackContext)
			return s.Savepoint(ctx, "one", func(child context.Context) error { return s.Scan(child, "SELECT 1", nil, &result) })
		}))
		legacy, err := stdsql.NewRunner(db, stdsql.NoScopePolicy(), stdsql.RunnerOptions{})
		require.NoError(t, err)
		require.NoError(t, legacy.Within(parent, scope, func(_ context.Context, s *stdsql.Session[struct{}]) error {
			return s.Scan(parent, "SELECT 1", nil, &result)
		}))
	})
	t.Run("pgx", func(t *testing.T) {
		r, err := pgxdriver.NewRunner(pool, pgxdriver.NoScopePolicy(), pgxdriver.RunnerOptions{RequireCallbackContext: true, ExposeTx: true, Unscoped: pgxdriver.UnscopedReadWrite})
		require.NoError(t, err)
		exerciseCallback(t, func(body, before callback) error {
			opts := pgxdriver.RunOptions[struct{}]{BeforeCommit: adapt[*pgxdriver.Session[struct{}]](before)}
			return r.WithinWith(t.Context(), scope, opts, adapt[*pgxdriver.Session[struct{}]](body))
		})
		checkRetry(t, func(fn callback) error {
			return r.WithinRetry(parent, scope, tabula.RetryConfig{MaxAttempts: 2, BaseDelay: time.Millisecond}, adapt[*pgxdriver.Session[struct{}]](fn))
		})
		require.NoError(t, r.WithinShared(parent, func(ctx context.Context, sh *pgxdriver.Shared[struct{}]) error {
			checkShared(t, ctx, sh.Scoped(scope))
			checkShared(t, ctx, sh.Unscoped())
			return nil
		}))
		type entry struct {
			ID    int64  `db:"id,key"`
			Value string `db:"value"`
		}
		reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
		table := tabula.Define(reg, tabula.TableSpec[entry, int64]{Name: "public.entries"})
		require.NoError(t, r.WithinRaw(parent, scope, func(ctx context.Context, s *pgxdriver.RawSession[struct{}]) error {
			checkShared(t, ctx, s)
			require.ErrorIs(t, s.Savepoint(parent, nil), tabula.ErrCallbackContext)
			return s.Savepoint(ctx, func(child context.Context, s *pgxdriver.Session[struct{}]) error {
				checkShared(t, child, s)
				_, err := pgxdriver.CopyFrom(parent, s, table, nil)
				require.ErrorIs(t, err, tabula.ErrCallbackContext)
				_, err = pgxdriver.CopyFrom(child, s, table, nil)
				return err
			})
		}))
		legacy, err := pgxdriver.NewRunner(pool, pgxdriver.NoScopePolicy(), pgxdriver.RunnerOptions{})
		require.NoError(t, err)
		require.NoError(t, legacy.Within(parent, scope, func(_ context.Context, s *pgxdriver.Session[struct{}]) error {
			return s.Scan(parent, "SELECT 1", nil, &result)
		}))
	})
}
