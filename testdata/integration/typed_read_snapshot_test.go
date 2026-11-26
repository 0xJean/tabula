//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/stdsql"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

type listTrace func(context.Context, q.TraceInfo) (context.Context, func(int64, error))

func (f listTrace) Start(ctx context.Context, info q.TraceInfo) (context.Context, func(int64, error)) {
	return f(ctx, info)
}

func TestReadListSnapshotAndLateFailureBothDrivers(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		for _, isolation := range []string{"committed", "repeatable", "cancelled"} {
			t.Run(driver+"/"+isolation, func(t *testing.T) {
				f := newListFixture(t, db, driver)
				plan := q.DefineTableRead(f.items, "snapshot", q.SelectColumns(f.id.Column()), q.Select().OrderBy(q.Asc(f.id)).WithList(q.ReadListSpec{ExactCount: true}))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var keys []string
				injected := false
				trace := listTrace(func(ctx context.Context, info q.TraceInfo) (context.Context, func(int64, error)) {
					keys = append(keys, info.Key)
					return ctx, func(_ int64, err error) {
						if strings.HasSuffix(info.Key, "list.count") && err == nil && !injected {
							injected = true
							if isolation == "cancelled" {
								cancel()
								return
							}
							_, err := db.ExecContext(t.Context(), "INSERT INTO "+f.schema+".items VALUES (10,1,'new',false)")
							require.NoError(t, err)
						}
					}
				})
				body := func(ctx context.Context, s q.Session[itemScope]) error {
					page, total, err := plan.Bind(s).List(ctx, q.RowChoice{})
					require.True(t, injected)
					if isolation == "cancelled" {
						require.Error(t, err)
						require.Nil(t, page)
						require.Zero(t, total)
						return err
					}
					require.NoError(t, err)
					require.EqualValues(t, 4, total)
					want := 4
					if isolation == "committed" {
						want = 5
					}
					require.Len(t, page, want)
					require.Len(t, keys, 2)
					require.Contains(t, keys[0], "list.count")
					require.Contains(t, keys[1], "list.data")
					return nil
				}
				var err error
				if driver == "stdsql" {
					runner, e := stdsql.NewRunner(db, stdsql.ScopePolicy[itemScope]{}, stdsql.RunnerOptions{Role: f.role, SQLAuthority: f.reg.SQLAuthority(), Tracer: trace})
					require.NoError(t, e)
					level := sql.LevelReadCommitted
					if isolation == "repeatable" {
						level = sql.LevelRepeatableRead
					}
					err = runner.WithinIsolation(ctx, 1, level, func(ctx context.Context, s *stdsql.Session[itemScope]) error { return body(ctx, s) })
				} else {
					runner, e := pgxdriver.NewRunner(pool, pgxdriver.ScopePolicy[itemScope]{}, pgxdriver.RunnerOptions{Role: f.role, SQLAuthority: f.reg.SQLAuthority(), Tracer: trace})
					require.NoError(t, e)
					level := pgx.ReadCommitted
					if isolation == "repeatable" {
						level = pgx.RepeatableRead
					}
					err = runner.WithinTxOptions(ctx, 1, pgx.TxOptions{IsoLevel: level}, func(ctx context.Context, s *pgxdriver.Session[itemScope]) error { return body(ctx, s) })
				}
				if isolation == "cancelled" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
			})
		}
	}
}
