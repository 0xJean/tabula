//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"iter"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/stdsql"
)

type operationRun func(context.Context, func(context.Context, tabula.Session[struct{}]) error) error

func operationRunner(t *testing.T, driver string, db *sql.DB, pool *pgxpool.Pool) operationRun {
	t.Helper()
	if driver == "stdsql" {
		r, err := stdsql.NewRunner(db, stdsql.NoScopePolicy(), stdsql.RunnerOptions{RequireCallbackContext: true})
		require.NoError(t, err)
		return func(ctx context.Context, body func(context.Context, tabula.Session[struct{}]) error) error {
			return r.Within(ctx, struct{}{}, func(ctx context.Context, s *stdsql.Session[struct{}]) error { return body(ctx, s) })
		}
	}
	r, err := pgxdriver.NewRunner(pool, pgxdriver.NoScopePolicy(), pgxdriver.RunnerOptions{RequireCallbackContext: true})
	require.NoError(t, err)
	return func(ctx context.Context, body func(context.Context, tabula.Session[struct{}]) error) error {
		return r.Within(ctx, struct{}{}, func(ctx context.Context, s *pgxdriver.Session[struct{}]) error { return body(ctx, s) })
	}
}

func TestGuardedOperationCleanup(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx := t.Context()
			ns := "tabula_operation_" + strings.ToLower(rand.Text())
			_, err := db.ExecContext(ctx, "CREATE SCHEMA "+ns+"; CREATE TABLE "+ns+".items(id integer PRIMARY KEY)")
			require.NoError(t, err)
			t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + ns + " CASCADE"); require.NoError(t, err) })
			run := operationRunner(t, driver, db, pool)
			count := func(want int) {
				t.Helper()
				var n int
				require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+ns+".items").Scan(&n))
				require.Equal(t, want, n)
			}
			t.Run("unfinished pull closes and rolls back", func(t *testing.T) {
				var next func() (int, error, bool)
				var stop func()
				err := run(ctx, func(parentCtx context.Context, s tabula.Session[struct{}]) error {
					_, err := s.Exec(parentCtx, "INSERT INTO "+ns+".items VALUES(1)")
					require.NoError(t, err)
					stream := func(yield func(int, error) bool) {
						err := s.(tabula.OperationSession[struct{}]).WithinOperation(parentCtx, func(childCtx context.Context, child tabula.Session[struct{}]) error {
							_, err := child.Exec(childCtx, "INSERT INTO "+ns+".items VALUES(2)")
							if err != nil {
								return err
							}
							yielding := true
							return child.Each(childCtx, "SELECT id FROM "+ns+".items ORDER BY id", nil, func(scan func(...any) error) error {
								if !yielding {
									return nil
								}
								var id int
								if err := scan(&id); err != nil {
									return err
								}
								yielding = yield(id, nil)
								return nil
							})
						})
						if err != nil {
							yield(0, err)
						}
					}
					next, stop = iter.Pull2(iter.Seq2[int, error](stream))
					id, err, ok := next()
					require.True(t, ok)
					require.NoError(t, err)
					require.Equal(t, 1, id)
					return nil
				})
				defer stop()
				require.ErrorIs(t, err, tabula.ErrSessionClosed)
				count(0)
				_, err, ok := next()
				require.True(t, ok)
				require.ErrorIs(t, err, tabula.ErrSessionClosed)
				_, _, ok = next()
				require.False(t, ok)
			})
			t.Run("ignored database failure forbids commit", func(t *testing.T) {
				err := run(ctx, func(parentCtx context.Context, s tabula.Session[struct{}]) error {
					_, err := s.Exec(parentCtx, "INSERT INTO "+ns+".items VALUES(3)")
					require.NoError(t, err)
					err = s.(tabula.OperationSession[struct{}]).WithinOperation(parentCtx, func(childCtx context.Context, child tabula.Session[struct{}]) error {
						_, err := child.Exec(childCtx, "SELECT 1 / 0")
						require.Equal(t, "22012", tabula.SQLState(err))
						return nil
					})
					require.Equal(t, "25P02", tabula.SQLState(err))
					_, err = s.Exec(parentCtx, "INSERT INTO "+ns+".items VALUES(4)")
					require.Equal(t, "25P02", tabula.SQLState(err))
					return nil
				})
				require.Equal(t, "25P02", tabula.SQLState(err))
				count(0)
			})
			t.Run("decode error rolls back only operation", func(t *testing.T) {
				require.NoError(t, run(ctx, func(parentCtx context.Context, s tabula.Session[struct{}]) error {
					err := s.(tabula.OperationSession[struct{}]).WithinOperation(parentCtx, func(childCtx context.Context, child tabula.Session[struct{}]) error {
						_, err := child.Exec(childCtx, "INSERT INTO "+ns+".items VALUES(5)")
						if err != nil {
							return err
						}
						return child.Each(childCtx, "SELECT 'invalid'::text", nil, func(scan func(...any) error) error { var n int; return scan(&n) })
					})
					require.Error(t, err)
					_, err = s.Exec(parentCtx, "INSERT INTO "+ns+".items VALUES(6)")
					return err
				}))
				count(1)
				var id int
				require.NoError(t, db.QueryRowContext(ctx, "SELECT id FROM "+ns+".items").Scan(&id))
				require.Equal(t, 6, id)
			})
		})
	}
}
