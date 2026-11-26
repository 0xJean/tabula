//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/stdsql"
	"github.com/stretchr/testify/require"
)

type operationItem struct {
	ID   int64  `db:"id,key"`
	Note string `db:"note"`
}

func TestGuardedOperationLifecycle(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx := t.Context()
			ns := "tabula_operation_" + strings.ToLower(rand.Text())
			_, err := db.ExecContext(ctx, "CREATE SCHEMA "+ns)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := db.Exec("DROP SCHEMA " + ns + " CASCADE")
				require.NoError(t, err)
			})
			_, err = db.ExecContext(ctx, "CREATE TABLE "+ns+".items (id bigint PRIMARY KEY, note text NOT NULL)")
			require.NoError(t, err)
			key := tabula.NewSessionKey()
			registry := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{SessionKey: key, Deny: tabula.DenyLegacySQL})
			table := tabula.Define(registry, tabula.TableSpec[operationItem, int64]{Name: ns + ".items", RelationScope: tabula.Global()})
			type body func(context.Context, tabula.Session[struct{}]) error
			var run, readOnly func(context.Context, body) error
			if driver == "stdsql" {
				runner, err := stdsql.NewRunner(db, stdsql.NoScopePolicy(), stdsql.RunnerOptions{
					SessionKey: key, SQLAuthority: registry.SQLAuthority(), RequireCallbackContext: true,
				})
				require.NoError(t, err)
				run = func(ctx context.Context, f body) error {
					return runner.Within(ctx, struct{}{}, func(ctx context.Context, s *stdsql.Session[struct{}]) error { return f(ctx, s) })
				}
				readOnly = func(ctx context.Context, f body) error {
					return runner.WithinReadOnly(ctx, struct{}{}, func(ctx context.Context, s *stdsql.Session[struct{}]) error { return f(ctx, s) })
				}
			} else {
				runner, err := pgxdriver.NewRunner(pool, pgxdriver.NoScopePolicy(), pgxdriver.RunnerOptions{
					SessionKey: key, SQLAuthority: registry.SQLAuthority(), RequireCallbackContext: true,
				})
				require.NoError(t, err)
				run = func(ctx context.Context, f body) error {
					return runner.Within(ctx, struct{}{}, func(ctx context.Context, s *pgxdriver.Session[struct{}]) error { return f(ctx, s) })
				}
				readOnly = func(ctx context.Context, f body) error {
					return runner.WithinReadOnly(ctx, struct{}{}, func(ctx context.Context, s *pgxdriver.Session[struct{}]) error { return f(ctx, s) })
				}
			}
			failure := errors.New("operation result failed")
			var retained tabula.Session[struct{}]
			var retainedCtx context.Context
			require.NoError(t, run(ctx, func(parentCtx context.Context, parent tabula.Session[struct{}]) error {
				ops, ok := parent.(tabula.OperationSession[struct{}])
				require.True(t, ok)
				require.Error(t, ops.WithinOperation(parentCtx, nil))
				require.NoError(t, table.Bind(parent).Insert(parentCtx, &operationItem{ID: 1, Note: "before"}))
				err := ops.WithinOperation(parentCtx, func(childCtx context.Context, child tabula.Session[struct{}]) error {
					retained, retainedCtx = child, childCtx
					require.NoError(t, table.Bind(child).Insert(childCtx, &operationItem{ID: 2, Note: "rolled back"}))
					_, err := table.Bind(parent).Get(parentCtx, 1)
					require.ErrorIs(t, err, tabula.ErrSessionBusy)
					_, err = table.Bind(child).Get(parentCtx, 1)
					require.ErrorIs(t, err, tabula.ErrCallbackContext)
					row, err := table.Bind(child).Get(childCtx, 1)
					require.NoError(t, err)
					require.Equal(t, int64(1), row.ID)
					return failure
				})
				require.ErrorIs(t, err, failure)
				missing, err := table.Bind(parent).Get(parentCtx, 2)
				require.NoError(t, err)
				require.Nil(t, missing)
				_, err = table.Bind(retained).Get(retainedCtx, 1)
				require.ErrorIs(t, err, tabula.ErrSessionClosed)
				require.NoError(t, ops.WithinOperation(parentCtx, func(childCtx context.Context, child tabula.Session[struct{}]) error {
					require.NoError(t, table.Bind(child).Insert(childCtx, &operationItem{ID: 3, Note: "outer"}))
					nested := child.(tabula.OperationSession[struct{}])
					err := nested.WithinOperation(childCtx, func(innerCtx context.Context, inner tabula.Session[struct{}]) error {
						require.NoError(t, table.Bind(inner).Insert(innerCtx, &operationItem{ID: 4, Note: "inner"}))
						return failure
					})
					require.ErrorIs(t, err, failure)
					missing, err := table.Bind(child).Get(childCtx, 4)
					require.NoError(t, err)
					require.Nil(t, missing)
					return nil
				}))
				require.PanicsWithValue(t, "operation stopped", func() {
					_ = ops.WithinOperation(parentCtx, func(childCtx context.Context, child tabula.Session[struct{}]) error {
						require.NoError(t, table.Bind(child).Insert(childCtx, &operationItem{ID: 5, Note: "panic"}))
						panic("operation stopped")
					})
				})
				missing, err = table.Bind(parent).Get(parentCtx, 5)
				require.NoError(t, err)
				require.Nil(t, missing)
				return nil
			}))
			var count int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+ns+".items").Scan(&count))
			require.Equal(t, 2, count)
			require.NoError(t, readOnly(ctx, func(parentCtx context.Context, parent tabula.Session[struct{}]) error {
				return parent.(tabula.OperationSession[struct{}]).WithinOperation(parentCtx, func(childCtx context.Context, child tabula.Session[struct{}]) error {
					_, err := table.Bind(child).Get(childCtx, 1)
					require.NoError(t, err)
					err = table.Bind(child).Insert(childCtx, &operationItem{ID: 6, Note: "denied"})
					require.ErrorIs(t, err, tabula.ErrReadOnly)
					return nil
				})
			}))
		})
	}
}
