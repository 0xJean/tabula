//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/0xJean/tabula/stdsql"
	"github.com/stretchr/testify/require"
)

type itemScope int64

type writableItem struct {
	ID       int64     `db:"id,key,serial"`
	Scope    itemScope `db:"scope_id,scope"`
	Label    string    `db:"label"`
	Rank     int64     `db:"rank"`
	Source   string    `db:"source,once"`
	Revision int       `db:"revision,version"`
	Removed  bool      `db:"removed,deleted"`
}

func TestTableWriteModeRoles(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx := t.Context()
			ns := fmt.Sprintf("tabula_modes_%d", time.Now().UnixNano())
			table := ns + ".items"
			appendRole, updateRole := ns+"_append", ns+"_update"
			exec := func(query string) {
				t.Helper()
				_, err := db.ExecContext(ctx, query)
				require.NoError(t, err)
			}
			for _, role := range []string{appendRole, updateRole} {
				exec("CREATE ROLE " + role)
				t.Cleanup(func() { _, err := db.Exec("DROP ROLE " + role); require.NoError(t, err) })
			}
			exec("CREATE SCHEMA " + ns)
			t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + ns + " CASCADE"); require.NoError(t, err) })
			exec("CREATE TABLE " + table + ` (
				id bigserial PRIMARY KEY, scope_id bigint NOT NULL, label text NOT NULL, rank bigint NOT NULL,
				source text NOT NULL, revision integer NOT NULL, removed boolean NOT NULL, UNIQUE(scope_id, label))`)
			exec("GRANT USAGE ON SCHEMA " + ns + " TO " + appendRole + ", " + updateRole)
			exec("GRANT INSERT (scope_id, label, rank, source, revision, removed) ON " + table + " TO " + appendRole)
			exec("GRANT USAGE ON SEQUENCE " + table + "_id_seq TO " + appendRole)
			exec("GRANT SELECT (id, scope_id, label, rank, source, revision, removed), UPDATE (rank, revision, removed) ON " + table + " TO " + updateRole)

			reg := tabula.NewRegistry(tabula.RegistryConfig[itemScope]{})
			appendTable := tabula.Define(reg, tabula.TableSpec[writableItem, int64]{Name: table, WriteMode: tabula.WriteInsertOnly})
			plan := appendTable.DefineUpsert("append_once", tabula.UpsertSpec{})
			updateTable := tabula.Define(reg, tabula.TableSpec[writableItem, int64]{
				Name: table, WriteMode: tabula.WriteUpdateOnly, UpdateColumns: []string{"rank", "revision", "removed"},
			})
			checkNeeds := func(def tabula.Definition, role string, missingGrants []string, missing ...sqlcheck.MissingRequirement) {
				t.Helper()
				needs, empty := sqlcheck.CollectPrivilegeNeeds([]tabula.Definition{def})
				require.Empty(t, empty)
				got, extra, err := sqlcheck.CheckRolePrivileges(ctx, db, needs, role)
				require.NoError(t, err)
				require.ElementsMatch(t, missingGrants, got)
				require.Empty(t, extra)
				findings, err := sqlcheck.CheckReadiness(ctx, db, role, sqlcheck.ReadinessNeeds{
					Privileges: needs, SchemaUsage: []string{ns},
				})
				require.NoError(t, err)
				require.ElementsMatch(t, missing, findings)
			}
			verify := func(def tabula.Definition, role string) []string {
				t.Helper()
				issues, err := sqlcheck.VerifyStatements(ctx, db, []tabula.Definition{def}, role)
				require.NoError(t, err)
				return issues
			}
			checkNeeds(appendTable.Definition(), appendRole, nil)
			checkNeeds(updateTable.Definition(), updateRole, nil)
			require.Empty(t, verify(appendTable.Definition(), appendRole))
			require.Empty(t, verify(updateTable.Definition(), updateRole))

			run := func(role string, body func(context.Context, tabula.Session[itemScope]) error) error {
				t.Helper()
				if driver == "stdsql" {
					r, err := stdsql.NewRunner(db, stdsql.ScopePolicy[itemScope]{}, stdsql.RunnerOptions{Role: role})
					require.NoError(t, err)
					return r.Within(ctx, 1, func(ctx context.Context, s *stdsql.Session[itemScope]) error { return body(ctx, s) })
				}
				r, err := pgxdriver.NewRunner(pool, pgxdriver.ScopePolicy[itemScope]{}, pgxdriver.RunnerOptions{Role: role})
				require.NoError(t, err)
				return r.Within(ctx, 1, func(ctx context.Context, s *pgxdriver.Session[itemScope]) error { return body(ctx, s) })
			}
			entry := func(label string) writableItem {
				return writableItem{Scope: 1, Label: label, Source: "seed", Revision: 1}
			}
			require.NoError(t, run(appendRole, func(ctx context.Context, s tabula.Session[itemScope]) error {
				rows := appendTable.Bind(s)
				row := entry("first")
				row.ID = -1
				require.NoError(t, rows.Insert(ctx, &row))
				require.EqualValues(t, -1, row.ID)
				require.NoError(t, rows.InsertMany(ctx, []writableItem{entry("second"), entry("third")}))
				changed, err := rows.Upsert(ctx, plan, &row)
				require.NoError(t, err)
				require.False(t, changed)
				n, err := rows.UpsertMany(ctx, plan, []writableItem{entry("second"), entry("fourth")})
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				if native, ok := s.(*pgxdriver.Session[itemScope]); ok {
					n, err := pgxdriver.CopyFrom(ctx, native, appendTable, []writableItem{entry("copy")})
					require.NoError(t, err)
					require.EqualValues(t, 1, n)
					bad := entry("wrong-scope")
					bad.Scope = 2
					_, err = pgxdriver.CopyFrom(ctx, native, appendTable, []writableItem{entry("unused"), bad})
					require.ErrorIs(t, err, tabula.ErrScopeMismatch)
				}
				return nil
			}))
			var key int64
			require.NoError(t, db.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE label='first'").Scan(&key))
			require.Positive(t, key)
			require.NoError(t, run(updateRole, func(ctx context.Context, s tabula.Session[itemScope]) error {
				rows := updateTable.Bind(s)
				row, err := rows.Lock(ctx, key)
				require.NoError(t, err)
				require.NotNil(t, row)
				row.Source = "changed"
				require.ErrorIs(t, rows.Save(ctx, *row), tabula.ErrOnce)
				row.Source, row.Label, row.Rank, row.Revision = "seed", "ignored", 3, 2
				require.NoError(t, rows.SaveReturning(ctx, row, "label"))
				require.Equal(t, "first", row.Label)
				require.ErrorIs(t, rows.Save(ctx, *row), tabula.ErrConflict)
				changed, err := rows.SetWhere(ctx, *row, []string{"rank"}, "revision = ?", 2)
				require.NoError(t, err)
				require.True(t, changed)
				require.NoError(t, rows.SetExpr(ctx, key, tabula.Add("rank", 1)))
				row, err = rows.Get(ctx, key)
				require.NoError(t, err)
				require.EqualValues(t, 4, row.Rank)
				row.Removed, row.Revision = true, 3
				require.NoError(t, rows.SoftDelete(ctx, *row))
				gone, err := rows.Get(ctx, key)
				require.NoError(t, err)
				require.Nil(t, gone)
				if native, ok := s.(*pgxdriver.Session[itemScope]); ok {
					_, err := pgxdriver.CopyFrom(ctx, native, updateTable, nil)
					require.ErrorIs(t, err, tabula.ErrOperationDenied)
					_, err = pgxdriver.CopyFrom(ctx, native, updateTable, []writableItem{entry("denied")})
					require.ErrorIs(t, err, tabula.ErrOperationDenied)
				}
				return nil
			}))

			appendOne := func(ctx context.Context, s tabula.Session[itemScope]) error {
				row := entry("needs-grant")
				return appendTable.Bind(s).Insert(ctx, &row)
			}
			assertDenied := func(err error) {
				t.Helper()
				require.Equal(t, "42501", tabula.SQLState(err))
				require.ErrorIs(t, err, tabula.ErrInsufficientPrivilege)
			}
			exec("REVOKE USAGE ON SEQUENCE " + table + "_id_seq FROM " + appendRole)
			checkNeeds(appendTable.Definition(), appendRole, []string{table + " USAGE(seq id)"},
				sqlcheck.MissingRequirement{Kind: "SEQUENCE", Object: table, Column: "id", Privilege: "USAGE"})
			assertDenied(run(appendRole, appendOne))
			exec("GRANT USAGE ON SEQUENCE " + table + "_id_seq TO " + appendRole)
			exec("REVOKE INSERT (source) ON " + table + " FROM " + appendRole)
			checkNeeds(appendTable.Definition(), appendRole, []string{table + " INSERT(source)"},
				sqlcheck.MissingRequirement{Kind: "COLUMN", Object: table, Column: "source", Privilege: "INSERT"})
			require.Len(t, verify(appendTable.Definition(), appendRole), 2)
			assertDenied(run(appendRole, appendOne))

			exec("REVOKE UPDATE (rank) ON " + table + " FROM " + updateRole)
			checkNeeds(updateTable.Definition(), updateRole, []string{table + " UPDATE(rank)"},
				sqlcheck.MissingRequirement{Kind: "COLUMN", Object: table, Column: "rank", Privilege: "UPDATE"})
			issues := verify(updateTable.Definition(), updateRole)
			require.Len(t, issues, 1)
			require.Contains(t, issues[0], table+" update:")
			assertDenied(run(updateRole, func(ctx context.Context, s tabula.Session[itemScope]) error {
				row := entry("second")
				return updateTable.Bind(s).Set(ctx, row, "rank")
			}))
		})
	}
}
