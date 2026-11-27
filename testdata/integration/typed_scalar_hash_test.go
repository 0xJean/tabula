//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/stretchr/testify/require"
)

func TestNativeHashTextAndDateTime(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		value, seed := q.Input("value", pg.Text()), q.Input("seed", pg.Int8())
		for _, text := range []string{"", "items", "caf\u00e9\u65e5"} {
			for _, initial := range []int64{0, -7} {
				var expected int64
				require.NoError(f.t, f.db.QueryRowContext(f.t.Context(), "SELECT pg_catalog.hashtextextended($1::text,$2::bigint)", text, initial).Scan(&expected))
				scalarCheck(f, "native_hash", pg.Int8(), q.HashTextExtended(value.Expr(), seed.Expr()), new(expected), q.BindValue(value, text), q.BindValue(seed, initial))
			}
		}
		scalarCheck(f, "null_hash", pg.Int8(), q.HashTextExtended(q.TypedNull(pg.Text()), seed.Expr()), nil, q.BindValue(seed, int64(-7)))
		scalarCheck(f, "null_seed", pg.Int8(), q.HashTextExtended(value.Expr(), q.TypedNull(pg.Int8())), nil, q.BindValue(value, "items"))
		view := q.DefineView(f.reg, q.ViewSpec[advisoryResult]{Name: "hash_lock"})
		lock := q.DefineAdvisoryLock(view, "key", q.AdvisoryXactLock(q.Exclusive, q.Try, q.HashTextExtended(value.Expr(), seed.Expr())))
		raw := effectSQLRunner(f.t, f.driver, f.db, f.pool, "")
		require.NoError(f.t, raw(f.t.Context(), func(ctx context.Context, holder session) error {
			require.NoError(f.t, holder.Scan(ctx, "SELECT TRUE FROM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1::text,$2::bigint))", []any{"caf\u00e9\u65e5", int64(-7)}, new(bool)))
			return f.run(f.t.Context(), func(ctx context.Context, other session) error {
				held, err := lock.Bind(other).Acquire(ctx, q.BindValue(value, "caf\u00e9\u65e5"), q.BindValue(seed, int64(-7)))
				require.NoError(f.t, err)
				require.False(f.t, held)
				return nil
			})
		}))
		day, clock := q.Input("day", pg.Date()), q.Input("clock", pg.Time())
		date := time.Date(2020, 2, 3, 0, 0, 0, 0, time.UTC)
		for _, expression := range []q.Expr[time.Time]{q.PlusAs(pg.Timestamp(), day.Expr(), clock.Expr()), q.PlusAs(pg.Timestamp(), clock.Expr(), day.Expr())} {
			plan := scalarPlan(f, pg.Timestamp(), expression)
			for _, at := range []struct {
				clock string
				value time.Time
			}{{"12:34:00", date.Add(12*time.Hour + 34*time.Minute)}, {"00:00:00", date}} {
				require.NoError(f.t, f.run(f.t.Context(), func(ctx context.Context, s session) error {
					rows, err := plan.Bind(s).Find(ctx, q.BindValue(day, date), q.BindValue(clock, at.clock))
					require.NoError(f.t, err)
					require.Len(f.t, rows, 1)
					require.NotNil(f.t, rows[0].Value)
					require.True(f.t, at.value.Equal(*rows[0].Value))
					return nil
				}))
			}
		}
		scalarCheck(f, "null_date", pg.Timestamp(), q.PlusAs(pg.Timestamp(), q.TypedNull(pg.Date()), clock.Expr()), nil, q.BindValue(clock, "00:00:00"))
		scalarCheck(f, "null_time", pg.Timestamp(), q.PlusAs(pg.Timestamp(), day.Expr(), q.TypedNull(pg.Time())), nil, q.BindValue(day, date))
	})
}
