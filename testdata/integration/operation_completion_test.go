//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestOperationCompletionCancellation(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		for _, mode := range []string{"normal", "raw", "shared", "before_commit"} {
			for _, visible := range []bool{false, true} {
				name := driver + "/" + mode + "/after_callback"
				if visible {
					name = driver + "/" + mode + "/callback_error"
				}
				t.Run(name, func(t *testing.T) {
					schema := typesSchema(t, db)
					_, err := db.ExecContext(t.Context(), "CREATE TABLE "+schema+".items(id integer PRIMARY KEY)")
					require.NoError(t, err)
					run := operationModeRunner(t, driver, mode, db, pool)
					err = run(t.Context(), func(ctx context.Context, parent tabula.Session[struct{}]) error {
						_, err := parent.Exec(ctx, "INSERT INTO "+schema+".items VALUES(1)")
						require.NoError(t, err)
						operationCtx, cancel := context.WithCancel(ctx)
						defer cancel()
						err = parent.(tabula.OperationSession[struct{}]).WithinOperation(operationCtx,
							func(childCtx context.Context, child tabula.Session[struct{}]) error {
								_, err := child.Exec(childCtx, "INSERT INTO "+schema+".items VALUES(2)")
								require.NoError(t, err)
								cancel()
								if visible {
									return childCtx.Err()
								}
								// The callback has completed successfully. The runtime
								// first observes cancellation after that boundary.
								return nil
							})
						require.ErrorIs(t, err, context.Canceled)
						_, err = parent.Exec(ctx, "INSERT INTO "+schema+".items VALUES(3)")
						if visible {
							require.NoError(t, err)
						} else {
							require.ErrorIs(t, err, context.Canceled)
						}
						return nil
					})
					if visible {
						require.NoError(t, err)
					} else {
						require.ErrorIs(t, err, context.Canceled)
					}
					var count int
					require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+schema+".items").Scan(&count))
					if visible {
						require.Equal(t, 2, count)
					} else {
						require.Zero(t, count)
					}
				})
			}
		}
	}
}
