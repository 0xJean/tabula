//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/plan"
	"github.com/stretchr/testify/require"
)

func TestSignedWriteRoutineMethods(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{})
			f := newEffectFixture(t, db, reg)
			policy := q.StatementPolicy{Read: []string{"select"}, Write: []string{"insert"}, RequireReturning: true}
			disabled := authorizedEffectRunner(t, driver, db, pool, f.role, reg.SQLAuthority(), policy, nil, false)
			require.NoError(t, disabled(t.Context(), func(ctx context.Context, s session) error {
				_, err := f.routines["add_one"].Bind(s).One(ctx, plan.BindValue(f.id, int64(10)))
				require.ErrorIs(t, err, q.ErrForbiddenStatement)
				return nil
			}))
			policy.AllowAuthorizedWriteRoutine = true
			run := authorizedEffectRunner(t, driver, db, pool, f.role, reg.SQLAuthority(), policy, nil, false)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				row, err := f.routines["add_one"].Bind(s).One(ctx, plan.BindValue(f.id, int64(10)))
				require.NoError(t, err)
				require.EqualValues(t, 10, row.ID)
				rows, err := f.routines["add_set"].Bind(s).All(ctx, plan.BindValue(f.id, int64(2)))
				require.NoError(t, err)
				require.Equal(t, []lockResult{{1}, {2}}, rows)
				text := f.view.Definition().Statements["add_one.call"]
				var id int64
				require.ErrorIs(t, s.Returning(ctx, text, []any{int64(19)}, &id), q.ErrForbiddenStatement)
				return nil
			}))
			readOnly := authorizedEffectRunner(t, driver, db, pool, f.role, reg.SQLAuthority(), policy, nil, true)
			require.NoError(t, readOnly(t.Context(), func(ctx context.Context, s session) error {
				_, err := f.routines["add_one"].Bind(s).One(ctx, plan.BindValue(f.id, int64(12)))
				require.ErrorIs(t, err, q.ErrReadOnly)
				return nil
			}))
			_, err := f.routines["add_one"].Bind(effectDirect(t, driver, reg.SQLAuthority())).One(t.Context(), plan.BindValue(f.id, int64(13)))
			require.ErrorIs(t, err, q.ErrReadOnly)
			blocked := errors.New("custom guard rejected statement")
			guarded := authorizedEffectRunner(t, driver, db, pool, f.role, reg.SQLAuthority(), policy, func(string) error { return blocked }, false)
			err = guarded(t.Context(), func(ctx context.Context, s session) error {
				_, err := f.routines["add_one"].Bind(s).One(ctx, plan.BindValue(f.id, int64(14)))
				return err
			})
			require.ErrorIs(t, err, blocked)
			var n int
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+f.schema+".entries").Scan(&n))
			require.Equal(t, 3, n)
		})
	}
}
