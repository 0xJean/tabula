//go:build integration

package integration_test

import (
	"context"
	"errors"
	"iter"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestWriteReturningCancellationAndLeakedPull(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			_, err := db.ExecContext(ctx, "CREATE TABLE "+schema+`.items(id bigint PRIMARY KEY, rank int NOT NULL, label text NOT NULL);
INSERT INTO `+schema+`.items SELECT n,0,'note' FROM generate_series(1,100) AS n`)
			require.NoError(t, err)
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			table := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".items", RelationScope: q.Global()})
			src := q.MapTable(table).As("i")
			id, rank := q.FieldOf(src.Rel(), "id", q.Int8()), q.FieldOf(src.Rel(), "rank", q.Int4())
			value := q.Input("rank", q.Int4())
			plan := q.DefineTableReturning(src, "change", q.UpdateOf(src).Set(q.Assign(rank, value.Expr())).AllRows(), q.SelectColumns(id.Column()))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			err = run(ctx, func(ctx context.Context, s session) error {
				cancelCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				for _, err := range plan.Bind(s).Iter(cancelCtx, q.BindValue(value, int32(1))) {
					if err != nil {
						return err
					}
					cancel()
				}
				return errors.New("canceled iterator did not report failure")
			})
			require.ErrorIs(t, err, context.Canceled)
			var total int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT sum(rank) FROM "+schema+".items").Scan(&total))
			require.Zero(t, total)
			var stop func()
			defer func() {
				if stop != nil {
					stop()
				}
			}()
			err = run(ctx, func(ctx context.Context, s session) error {
				next, closeRows := iter.Pull2(plan.Bind(s).Iter(ctx, q.BindValue(value, int32(2))))
				stop = closeRows
				_, err, ok := next()
				require.True(t, ok)
				require.NoError(t, err)
				return nil // A suspended operation must make commit fail.
			})
			require.Error(t, err)
			stop()
			stop = nil
			require.NoError(t, db.QueryRowContext(ctx, "SELECT sum(rank) FROM "+schema+".items").Scan(&total))
			require.Zero(t, total)
		})
	}
}

func TestWriteVisibleErrorAndCancellationAfterBreak(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			_, err := db.ExecContext(ctx, "CREATE TABLE "+schema+`.items(id bigint PRIMARY KEY, rank int NOT NULL, label text NOT NULL);
INSERT INTO `+schema+`.items SELECT n,0,'note' FROM generate_series(1,2048) AS n;
CREATE FUNCTION `+schema+`.fail_items() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION USING ERRCODE='23514', CONSTRAINT='items_rank_check', MESSAGE='late failure'; END $$;
CREATE TRIGGER fail_items AFTER UPDATE ON `+schema+`.items FOR EACH STATEMENT EXECUTE FUNCTION `+schema+`.fail_items()`)
			require.NoError(t, err)
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			table := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".items", RelationScope: q.Global()})
			src := q.MapTable(table).As("i")
			id, rank := q.FieldOf(src.Rel(), "id", q.Int8()), q.FieldOf(src.Rel(), "rank", q.Int4())
			value := q.Input("rank", q.Int4())
			plan := q.DefineTableReturning(src, "change", q.UpdateOf(src).Set(q.Assign(rank, value.Expr())).AllRows(), q.SelectColumns(id.Column()))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			seen := 0
			var visible error
			err = run(ctx, func(ctx context.Context, s session) error {
				for _, err := range plan.Bind(s).Iter(ctx, q.BindValue(value, int32(1))) {
					if err != nil {
						visible = err
						return nil // This observed operation failure is recoverable.
					}
					seen++
				}
				return errors.New("expected statement trigger failure")
			})
			require.NoError(t, err)
			require.Zero(t, seen)
			require.NotErrorIs(t, visible, q.ErrUnobservedWrite)
			require.Equal(t, "23514", q.SQLState(visible))
			require.Equal(t, "items_rank_check", q.ConstraintName(visible))
			_, err = db.ExecContext(ctx, "DROP TRIGGER fail_items ON "+schema+".items")
			require.NoError(t, err)
			err = run(ctx, func(ctx context.Context, s session) error {
				operationCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				for _, err := range plan.Bind(s).Iter(operationCtx, q.BindValue(value, int32(1))) {
					require.NoError(t, err)
					seen++
					cancel()
					break
				}
				return nil // The canceled drain cannot yield another error.
			})
			require.Equal(t, 1, seen)
			require.ErrorIs(t, err, q.ErrUnobservedWrite)
			require.ErrorIs(t, err, context.Canceled)
			var total int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT sum(rank) FROM "+schema+".items").Scan(&total))
			require.Zero(t, total)
		})
	}
}
