//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestMultipleRowLocks(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			schema := typesSchema(t, db)
			role := fmt.Sprintf("scope_reader_%d", time.Now().UnixNano())
			exec := func(query string) { t.Helper(); _, err := db.ExecContext(t.Context(), query); require.NoError(t, err) }
			exec("CREATE ROLE " + role)
			t.Cleanup(func() { _, err := db.Exec("DROP OWNED BY " + role + "; DROP ROLE " + role); require.NoError(t, err) })
			exec("GRANT USAGE ON SCHEMA " + schema + " TO " + role)
			type item struct {
				ID   int64  `db:"id,key"`
				Note string `db:"note"`
			}
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{})
			makeSource := func(name, alias string) q.RowSource[item, struct{}] {
				exec("CREATE TABLE " + schema + "." + name + " (id bigint PRIMARY KEY, note text NOT NULL)")
				exec("INSERT INTO " + schema + "." + name + " VALUES (1, 'open')")
				exec("GRANT SELECT(id), UPDATE(note) ON " + schema + "." + name + " TO " + role)
				exec("ALTER TABLE " + schema + "." + name + " ENABLE ROW LEVEL SECURITY")
				exec("ALTER TABLE " + schema + "." + name + " FORCE ROW LEVEL SECURITY")
				exec("CREATE POLICY read_rows ON " + schema + "." + name + " FOR SELECT TO " + role + " USING (true)")
				exec("CREATE POLICY lock_rows ON " + schema + "." + name + " FOR UPDATE TO " + role + " USING (true)")
				return q.MapTable(q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: schema + "." + name, RelationScope: q.ByRLS()})).As(alias)
			}
			a, b := makeSource("items", "i"), makeSource("notes", "n")
			id, other := q.FieldOf(a.Rel(), "id", q.Int8()), q.FieldOf(b.Rel(), "id", q.Int8())
			view := q.DefineView(reg, q.ViewSpec[lockResult]{Name: "entries"})
			base := q.Select(q.Project(q.Output(view, "id", q.Int8()), id.Expr())).From(a).InnerJoin(b, q.Eq(id.Expr(), other.Expr()))
			build := func(name string, wait q.LockWait) *q.ReadPlan[lockResult, struct{}] {
				return q.DefineRead(view, name, base.Locks(q.ForRows(q.ForUpdate, a.Rel(), wait), q.ForRows(q.ForShare, b.Rel(), wait)))
			}
			locked, skip := build("locked", q.NoWait), build("skip", q.SkipLocked)
			run := authorizedEffectRunner(t, driver, db, pool, role, reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			raw := effectSQLRunner(t, driver, db, pool, role)
			read := func(plan *q.ReadPlan[lockResult, struct{}]) ([]lockResult, error) {
				var rows []lockResult
				err := run(t.Context(), func(ctx context.Context, s session) error {
					var err error
					rows, err = plan.Bind(s).Find(ctx)
					return err
				})
				return rows, err
			}
			rows, err := read(locked)
			require.NoError(t, err)
			require.Equal(t, []lockResult{{ID: 1}}, rows)
			for i, strength := range []q.RowLockStrength{q.ForUpdate, q.ForNoKeyUpdate, q.ForShare, q.ForKeyShare} {
				for j, wait := range []q.LockWait{q.Wait, q.NoWait, q.SkipLocked} {
					plan := q.DefineRead(view, fmt.Sprintf("mode_%d_%d", i, j), base.Locks(q.ForRows(strength, a.Rel(), wait), q.ForRows(strength, b.Rel(), wait)))
					rows, err := read(plan)
					require.NoError(t, err)
					require.Len(t, rows, 1)
				}
			}
			query := view.Definition().Statements["locked.rls.data"]
			// Native savepoint rollback releases both locks acquired inside it.
			var command func(string) error
			var rollback func() error
			if driver == "pq" {
				tx, err := db.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				command = func(text string) error { _, err := tx.ExecContext(t.Context(), text); return err }
				rollback = tx.Rollback
			} else {
				tx, err := pool.Begin(t.Context())
				require.NoError(t, err)
				command = func(text string) error { _, err := tx.Exec(t.Context(), text); return err }
				rollback = func() error { return tx.Rollback(t.Context()) }
			}
			defer func() { _ = rollback() }()
			require.NoError(t, command("SAVEPOINT locks"))
			require.NoError(t, command(query))
			_, err = read(locked)
			require.Equal(t, "55P03", q.SQLState(err))
			require.NoError(t, command("ROLLBACK TO SAVEPOINT locks"))
			_, err = read(locked)
			require.NoError(t, err)
			require.NoError(t, command(query))
			require.NoError(t, rollback())
			_, err = read(locked)
			require.NoError(t, err)
			for _, target := range []string{"items", "notes"} {
				require.NoError(t, raw(t.Context(), func(ctx context.Context, holder session) error {
					require.NoError(t, holder.Scan(ctx, "SELECT id FROM "+schema+"."+target+" FOR UPDATE", nil, new(int64)))
					_, err := read(locked)
					require.Equal(t, "55P03", q.SQLState(err), target)
					rows, err := read(skip)
					require.NoError(t, err)
					require.Empty(t, rows)
					// Parse/Describe the original multiple-lock statement while
					// both possible targets are contended. No Execute is sent.
					conn, err := pool.Acquire(t.Context())
					require.NoError(t, err)
					defer conn.Release()
					desc, err := conn.Conn().Prepare(t.Context(), "", query)
					require.NoError(t, err)
					require.Len(t, desc.Fields, 1)
					require.Equal(t, uint32(20), desc.Fields[0].DataTypeOID)
					return nil
				}))
				rows, err = read(locked)
				require.NoError(t, err)
				require.Len(t, rows, 1)
				exec("REVOKE UPDATE(note) ON " + schema + "." + target + " FROM " + role)
				_, err = read(locked)
				require.Equal(t, "42501", q.SQLState(err))
				work := sqlcheck.RoleStatements{}
				work.AddViews(role, view.Definition())
				needs, err := work.Readiness(role)
				require.NoError(t, err)
				missing, err := sqlcheck.CheckReadiness(t.Context(), db, role, needs)
				require.NoError(t, err)
				require.Contains(t, missing, sqlcheck.MissingRequirement{Kind: "TABLE", Object: `"` + schema + `"."` + target + `"`, Privilege: "UPDATE ANY COLUMN"})
				exec("GRANT UPDATE(note) ON " + schema + "." + target + " TO " + role)
				exec("ALTER POLICY lock_rows ON " + schema + "." + target + " USING (false)")
				rows, err = read(locked)
				require.NoError(t, err)
				require.Empty(t, rows, "each target's UPDATE policy is applied")
				exec("ALTER POLICY lock_rows ON " + schema + "." + target + " USING (true)")
			}
			ro := authorizedEffectRunner(t, driver, db, pool, role, reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			require.NoError(t, ro(t.Context(), func(ctx context.Context, s session) error {
				_, err := locked.Bind(s).Find(ctx)
				require.ErrorIs(t, err, q.ErrReadOnly)
				return nil
			}))
			_, err = locked.Bind(effectDirect(t, driver, reg.SQLAuthority())).Find(t.Context())
			require.ErrorIs(t, err, q.ErrReadOnly)
			if driver == "pq" {
				tx, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
				require.NoError(t, err)
				err = tx.QueryRowContext(t.Context(), query).Scan(new(int64))
				require.Equal(t, "25006", q.SQLState(err))
				require.NoError(t, tx.Rollback())
			} else {
				tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly})
				require.NoError(t, err)
				err = tx.QueryRow(t.Context(), query).Scan(new(int64))
				require.Equal(t, "25006", q.SQLState(err))
				require.NoError(t, tx.Rollback(t.Context()))
			}
		})
	}
}
