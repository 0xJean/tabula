//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

func TestSharedAdvisoryOverloads(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{})
			view := q.DefineView(reg, q.ViewSpec[advisoryResult]{Name: "entries"})
			big, first, second := q.Input("key", q.Int8()), q.Input("first", q.Int4()), q.Input("second", q.Int4())
			key := time.Now().UnixNano()
			readOnly := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			direct := effectDirect(t, driver, reg.SQLAuthority())
			for _, pair := range []bool{false, true} {
				name := "big"
				args := []q.Binding{q.BindValue(big, key)}
				if pair {
					name = "pair"
					args = []q.Binding{q.BindValue(first, int32(key>>32)), q.BindValue(second, int32(key))}
				}
				declare := func(label string, mode q.AdvisoryMode, wait q.LockWait) *q.AdvisoryLockPlan[advisoryResult, struct{}] {
					lock := q.AdvisoryXactLock(mode, wait, big.Expr())
					if pair {
						lock = q.AdvisoryXactLockPair(mode, wait, first.Expr(), second.Expr())
					}
					return q.DefineAdvisoryLock(view, name+"_"+label, lock)
				}
				sharedWait, sharedTry := declare("shared_wait", q.Shared, q.Wait), declare("shared_try", q.Shared, q.Try)
				exclusive := declare("exclusive", q.Exclusive, q.Try)
				for _, lock := range []*q.AdvisoryLockPlan[advisoryResult, struct{}]{sharedWait, sharedTry} {
					_, err := lock.Bind(direct).Acquire(t.Context(), args...)
					require.ErrorIs(t, err, q.ErrReadOnly)
				}
				rollback := errors.New("rollback holder")
				for _, end := range []error{nil, rollback} {
					err := readOnly(t.Context(), func(ctx context.Context, holder session) error {
						held, err := sharedWait.Bind(holder).Acquire(ctx, args...)
						require.NoError(t, err)
						require.True(t, held)
						require.NoError(t, readOnly(t.Context(), func(ctx context.Context, peer session) error {
							for _, lock := range []*q.AdvisoryLockPlan[advisoryResult, struct{}]{sharedWait, sharedTry} {
								held, err := lock.Bind(peer).Acquire(ctx, args...)
								require.NoError(t, err)
								require.True(t, held)
							}
							return run(t.Context(), func(ctx context.Context, other session) error {
								held, err := exclusive.Bind(other).Acquire(ctx, args...)
								require.NoError(t, err)
								require.False(t, held)
								return nil
							})
						}))
						return end
					})
					require.ErrorIs(t, err, end)
					require.NoError(t, run(t.Context(), func(ctx context.Context, holder session) error {
						held, err := exclusive.Bind(holder).Acquire(ctx, args...)
						require.NoError(t, err)
						require.True(t, held)
						return readOnly(t.Context(), func(ctx context.Context, other session) error {
							held, err := sharedTry.Bind(other).Acquire(ctx, args...)
							require.NoError(t, err)
							require.False(t, held)
							return nil
						})
					}))
				}
			}
		})
	}
}

func TestSharedAdvisoryNativePrivileges(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{})
			view := q.DefineView(reg, q.ViewSpec[advisoryResult]{Name: "entries"})
			role := fmt.Sprintf("scope_reader_%d", time.Now().UnixNano())
			exec := func(sql string) { t.Helper(); _, err := db.ExecContext(t.Context(), sql); require.NoError(t, err) }
			exec("CREATE ROLE " + role)
			t.Cleanup(func() { _, err := db.Exec("DROP OWNED BY " + role + "; DROP ROLE " + role); require.NoError(t, err) })
			run := authorizedEffectRunner(t, driver, db, pool, role, reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			big, small := q.Input("key", q.Int8()), q.Input("small", q.Int4())
			for i, lock := range []q.AdvisoryLock{
				q.AdvisoryXactLock(q.Shared, q.Wait, big.Expr()), q.AdvisoryXactLock(q.Shared, q.Try, big.Expr()),
				q.AdvisoryXactLockPair(q.Shared, q.Wait, small.Expr(), small.Expr()), q.AdvisoryXactLockPair(q.Shared, q.Try, small.Expr(), small.Expr()),
			} {
				name := fmt.Sprintf("lock_%d", i)
				plan := q.DefineAdvisoryLock(view, name, lock)
				args := []q.Binding{q.BindValue(big, time.Now().UnixNano())}
				if i >= 2 {
					args = []q.Binding{q.BindValue(small, int32(17))}
				}
				signature := view.Definition().StatementShapes[name+".lock"].Privileges[0].Function
				execute := func() error {
					return run(t.Context(), func(ctx context.Context, s session) error { _, err := plan.Bind(s).Acquire(ctx, args...); return err })
				}
				require.NoError(t, execute())
				exec("REVOKE EXECUTE ON FUNCTION " + signature + " FROM PUBLIC")
				err := execute()
				work := sqlcheck.RoleStatements{}
				work.AddViews(role, view.Definition())
				needs, needsErr := work.Readiness(role)
				missing, checkErr := sqlcheck.CheckReadiness(t.Context(), db, role, needs)
				exec("GRANT EXECUTE ON FUNCTION " + signature + " TO PUBLIC")
				require.Equal(t, "42501", q.SQLState(err))
				require.NoError(t, needsErr)
				require.NoError(t, checkErr)
				require.Len(t, missing, 1)
				require.Equal(t, "EXECUTE", missing[0].Privilege)
			}
		})
	}
}
