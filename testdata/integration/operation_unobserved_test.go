//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/stdsql"
)

func operationModeRunner(t *testing.T, driver, mode string, db *sql.DB, pool *pgxpool.Pool) operationRun {
	t.Helper()
	if driver == "stdsql" {
		r, err := stdsql.NewRunner(db, stdsql.NoScopePolicy(), stdsql.RunnerOptions{ExposeTx: true, Unscoped: stdsql.UnscopedReadWrite})
		require.NoError(t, err)
		return func(ctx context.Context, body func(context.Context, tabula.Session[struct{}]) error) error {
			invoke := func(ctx context.Context, s *stdsql.Session[struct{}]) error { return body(ctx, s) }
			switch mode {
			case "raw":
				return r.WithinRaw(ctx, struct{}{}, func(ctx context.Context, s *stdsql.RawSession[struct{}]) error { return body(ctx, s) })
			case "shared":
				return r.WithinShared(ctx, func(ctx context.Context, s *stdsql.Shared[struct{}]) error { return body(ctx, s.Scoped(struct{}{})) })
			case "before_commit":
				return r.WithinWith(ctx, struct{}{}, stdsql.RunOptions[struct{}]{BeforeCommit: invoke}, func(context.Context, *stdsql.Session[struct{}]) error { return nil })
			default:
				return r.Within(ctx, struct{}{}, invoke)
			}
		}
	}
	r, err := pgxdriver.NewRunner(pool, pgxdriver.NoScopePolicy(), pgxdriver.RunnerOptions{ExposeTx: true, Unscoped: pgxdriver.UnscopedReadWrite})
	require.NoError(t, err)
	return func(ctx context.Context, body func(context.Context, tabula.Session[struct{}]) error) error {
		invoke := func(ctx context.Context, s *pgxdriver.Session[struct{}]) error { return body(ctx, s) }
		switch mode {
		case "raw":
			return r.WithinRaw(ctx, struct{}{}, func(ctx context.Context, s *pgxdriver.RawSession[struct{}]) error { return body(ctx, s) })
		case "shared":
			return r.WithinShared(ctx, func(ctx context.Context, s *pgxdriver.Shared[struct{}]) error { return body(ctx, s.Scoped(struct{}{})) })
		case "before_commit":
			return r.WithinWith(ctx, struct{}{}, pgxdriver.RunOptions[struct{}]{BeforeCommit: invoke}, func(context.Context, *pgxdriver.Session[struct{}]) error { return nil })
		default:
			return r.Within(ctx, struct{}{}, invoke)
		}
	}
}

func TestUnobservedOperationFailure(t *testing.T) {
	db, pool := databases(t)
	ctx := t.Context()
	ns := "tabula_operation_" + strings.ToLower(rand.Text())
	_, err := db.ExecContext(ctx, "CREATE SCHEMA "+ns+"; CREATE TABLE "+ns+".items(id integer PRIMARY KEY, rank integer CONSTRAINT items_rank CHECK(rank > 0))")
	require.NoError(t, err)
	t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + ns + " CASCADE"); require.NoError(t, err) })
	callbackErr := errors.New("callback failed")
	for _, driver := range []string{"stdsql", "pgx"} {
		for _, mode := range []string{"normal", "raw", "shared", "before_commit"} {
			for _, extra := range []bool{false, true} {
				name := driver + "/" + mode
				if extra {
					name += "/callback_error"
				}
				t.Run(name, func(t *testing.T) {
					run := operationModeRunner(t, driver, mode, db, pool)
					var original error
					err := run(t.Context(), func(ctx context.Context, parent tabula.Session[struct{}]) error {
						_, err := parent.Exec(ctx, "INSERT INTO "+ns+".items VALUES(1,1)")
						require.NoError(t, err)
						err = parent.(tabula.OperationSession[struct{}]).WithinOperation(ctx, func(ctx context.Context, child tabula.Session[struct{}]) error {
							_, err := child.Exec(ctx, "INSERT INTO "+ns+".items VALUES(2,1)")
							require.NoError(t, err)
							_, original = child.Exec(ctx, "INSERT INTO "+ns+".items VALUES(3,-1)")
							require.Equal(t, "23514", tabula.SQLState(original))
							// The returning iterator emits this marker only after its
							// consumer stops. Here isolate the driver's failure boundary.
							return errors.Join(tabula.ErrUnobservedWrite, original)
						})
						require.ErrorIs(t, err, tabula.ErrUnobservedWrite)
						require.ErrorIs(t, err, original)
						_, err = parent.Exec(ctx, "INSERT INTO "+ns+".items VALUES(4,1)")
						require.ErrorIs(t, err, tabula.ErrUnobservedWrite)
						if extra {
							return callbackErr
						}
						return nil
					})
					require.ErrorIs(t, err, tabula.ErrUnobservedWrite)
					require.ErrorIs(t, err, original)
					require.Equal(t, "23514", tabula.SQLState(err))
					require.Equal(t, "items_rank", tabula.ConstraintName(err))
					if extra {
						require.ErrorIs(t, err, callbackErr)
					}
					var count int
					require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+ns+".items").Scan(&count))
					require.Zero(t, count)
				})
			}
		}
		t.Run(driver+"/nested", func(t *testing.T) {
			run := operationRunner(t, driver, db, pool)
			err := run(t.Context(), func(ctx context.Context, s tabula.Session[struct{}]) error {
				err := s.(tabula.OperationSession[struct{}]).WithinOperation(ctx, func(ctx context.Context, child tabula.Session[struct{}]) error {
					err := child.(tabula.OperationSession[struct{}]).WithinOperation(ctx, func(context.Context, tabula.Session[struct{}]) error {
						return errors.Join(tabula.ErrUnobservedWrite, callbackErr)
					})
					require.ErrorIs(t, err, tabula.ErrUnobservedWrite)
					return nil
				})
				require.ErrorIs(t, err, tabula.ErrUnobservedWrite)
				return nil
			})
			require.ErrorIs(t, err, tabula.ErrUnobservedWrite)
			require.ErrorIs(t, err, callbackErr)
		})
	}
}
