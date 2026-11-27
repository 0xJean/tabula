//go:build integration

package pgxdriver_test

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/plan"
)

func TestOperationAdapterBoundaries(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	ns := "tabula_operation_" + strings.ToLower(rand.Text())
	_, err = pool.Exec(ctx, "CREATE SCHEMA "+ns+"; CREATE TABLE "+ns+".items(id bigint PRIMARY KEY)")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA "+ns+" CASCADE")
		require.NoError(t, err)
	})
	key := tabula.NewSessionKey()
	type item struct {
		ID int64 `db:"id,key"`
	}
	type result struct {
		ID int64 `db:"id"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[string]{SessionKey: key})
	table := tabula.DefinePlanSource(reg, tabula.TableSpec[item, int64]{Name: ns + ".items", RelationScope: tabula.Global()})
	rel := plan.FromTable(table, "i", tabula.Global())
	id := plan.FieldOf(rel, "id", plan.Int8())
	view := tabula.DefineView(reg, tabula.ViewSpec[result]{Name: ns + ".result"})
	rows := plan.DefineRead(view, "read", plan.Select(plan.Project(plan.Output(view, "id", plan.Int8()), id.Expr())).From(rel))
	policy := pgxdriver.ScopePolicy[string]{ManagedVars: []string{"demo.scope"}, ScopeVars: func(s string) map[string]string { return map[string]string{"demo.scope": s} }}
	runner, err := pgxdriver.NewRunner(pool, policy, pgxdriver.RunnerOptions{ExposeTx: true, SessionKey: key, Unscoped: pgxdriver.UnscopedReadWrite, SQLAuthority: reg.SQLAuthority()})
	require.NoError(t, err)
	failure := errors.New("operation rejected")
	var retained tabula.Session[string]
	var retainedCtx context.Context
	require.NoError(t, runner.WithinRaw(ctx, "a", func(parentCtx context.Context, raw *pgxdriver.RawSession[string]) error {
		err := raw.WithinOperation(parentCtx, func(childCtx context.Context, child tabula.Session[string]) error {
			retained, retainedCtx = child, childCtx
			var scope string
			require.ErrorIs(t, child.Scan(parentCtx, "SELECT current_setting('demo.scope')", nil, &scope), tabula.ErrCallbackContext)
			require.NoError(t, child.Scan(childCtx, "SELECT current_setting('demo.scope')", nil, &scope))
			require.Equal(t, "a", scope)
			require.ErrorIs(t, raw.Scan(parentCtx, "SELECT 1", nil, new(int)), tabula.ErrSessionBusy)
			return child.(*pgxdriver.Session[string]).Savepoint(childCtx, func(spCtx context.Context, nested *pgxdriver.Session[string]) error {
				require.NoError(t, nested.WithinOperation(spCtx, func(opCtx context.Context, op tabula.Session[string]) error {
					_, err := op.Exec(opCtx, "INSERT INTO "+ns+".items VALUES(1)")
					if err != nil {
						return err
					}
					got, err := rows.Bind(op).Find(opCtx)
					require.NoError(t, err)
					require.Len(t, got, 1)
					return nil
				}))
				return failure
			})
		})
		require.ErrorIs(t, err, failure)
		_, err = rows.Bind(retained).Find(retainedCtx)
		require.ErrorIs(t, err, tabula.ErrSessionClosed)
		canceled, cancel := context.WithCancel(parentCtx)
		err = raw.WithinOperation(canceled, func(childCtx context.Context, child tabula.Session[string]) error {
			_, err := child.Exec(childCtx, "INSERT INTO "+ns+".items VALUES(2)")
			require.NoError(t, err)
			cancel()
			// Returning the observed cancellation keeps this operation recoverable.
			return childCtx.Err()
		})
		require.ErrorIs(t, err, context.Canceled)
		_, err = raw.Exec(parentCtx, "INSERT INTO "+ns+".items VALUES(3)")
		return err
	}))
	require.NoError(t, runner.WithinShared(ctx, func(parentCtx context.Context, shared *pgxdriver.Shared[string]) error {
		a, b := shared.Scoped("a"), shared.Scoped("b")
		return a.WithinOperation(parentCtx, func(childCtx context.Context, child tabula.Session[string]) error {
			var scope string
			require.NoError(t, child.Scan(childCtx, "SELECT current_setting('demo.scope')", nil, &scope))
			require.Equal(t, "a", scope)
			require.ErrorIs(t, b.Scan(parentCtx, "SELECT 1", nil, new(int)), tabula.ErrSessionBusy)
			return nil
		})
	}))
	require.NoError(t, runner.WithinWith(ctx, "a", pgxdriver.RunOptions[string]{BeforeCommit: func(stepCtx context.Context, s *pgxdriver.Session[string]) error {
		return s.WithinOperation(stepCtx, func(childCtx context.Context, child tabula.Session[string]) error {
			_, err := child.Exec(childCtx, "INSERT INTO "+ns+".items VALUES(4)")
			return err
		})
	}}, func(context.Context, *pgxdriver.Session[string]) error { return nil }))
	require.NoError(t, runner.WithinReadOnly(ctx, "a", func(parentCtx context.Context, s *pgxdriver.Session[string]) error {
		err := s.WithinOperation(parentCtx, func(childCtx context.Context, child tabula.Session[string]) error {
			got, err := rows.Bind(child).Find(childCtx)
			require.NoError(t, err)
			require.Len(t, got, 2)
			_, err = child.Exec(childCtx, "INSERT INTO "+ns+".items VALUES(5)")
			require.ErrorIs(t, err, tabula.ErrReadOnly)
			return err
		})
		require.ErrorIs(t, err, tabula.ErrReadOnly)
		require.Equal(t, "25006", tabula.SQLState(err))
		got, err := rows.Bind(s).Find(parentCtx)
		require.NoError(t, err)
		require.Len(t, got, 2)
		return nil
	}))
	var n int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+ns+".items WHERE id IN (3,4)").Scan(&n))
	require.Equal(t, 2, n)
}
