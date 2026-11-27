//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

func TestRegisteredLockedPageAndExists(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			f := newEffectFixture(t, db, q.NewRegistry(q.RegistryConfig[struct{}]{}))
			_, err := db.ExecContext(ctx, "INSERT INTO "+f.table+" VALUES (3, false, 'ready'), (4, false, NULL)")
			require.NoError(t, err)

			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			type item struct {
				ID      int64   `db:"id,key"`
				Removed bool    `db:"removed,deleted"`
				Note    *string `db:"note"`
			}
			table := q.Define(reg, q.TableSpec[item, int64]{Name: f.table, RelationScope: q.Global()})
			view := q.DefineView(reg, q.ViewSpec[lockResult]{Name: "entries"})
			rel := plan.FromTable(table, "i", q.Global())
			src := plan.RowsFrom(table, rel)
			id, note := plan.FieldOf(rel, "id", plan.Int8()), plan.FieldOf(rel, "note", plan.Text())
			key := plan.Input("key", plan.Int8())
			projection := plan.Project(plan.Output(view, "id", plan.Int8()), id.Expr())
			lock := plan.DefineRows(view, "take", src, plan.RowsSpec{
				Lock:      plan.ForRows(plan.ForUpdate, rel, plan.SkipLocked),
				RowFilter: plan.RowFilter{Where: plan.Ge(id.Expr(), key.Expr())},
				Orders:    []plan.RowOrder{{Name: "next", Terms: []plan.RowTerm{plan.Asc(id)}}},
				Pages:     plan.RowPages{Keyset: true, Offset: true, MaxLimit: 1, MaxOffset: 4},
			}, projection)
			work := make(sqlcheck.RoleStatements)
			work.AddViews(f.role, view.Definition())
			require.Len(t, work[f.role], 3)
			for _, stmt := range work[f.role] {
				require.Contains(t, stmt.SQL, " ORDER BY ")
				require.Contains(t, stmt.SQL, " LIMIT ")
				require.Contains(t, stmt.SQL, ` FOR UPDATE OF "i" SKIP LOCKED`)
			}
			needs, err := work.Readiness(f.role)
			require.NoError(t, err)
			missing, err := sqlcheck.CheckReadiness(ctx, db, f.role, needs)
			require.NoError(t, err)
			require.Empty(t, missing)
			findings, err := sqlcheck.VerifyRoleStatements(ctx, db, work)
			require.NoError(t, err)
			require.Empty(t, findings)

			run := authorizedEffectRunner(t, driver, db, pool, f.role, reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			first := plan.RowChoice{Order: "next", Page: plan.FirstPage, Limit: 1}
			lower := plan.BindValue(key, 1)
			require.NoError(t, run(ctx, func(c context.Context, s session) error {
				rows, err := lock.Bind(s).Find(c, first, lower)
				require.NoError(t, err)
				require.Equal(t, []lockResult{{1}}, rows)
				return run(ctx, func(c context.Context, other session) error {
					rows, err := lock.Bind(other).Find(c, first, lower)
					require.NoError(t, err)
					require.Equal(t, []lockResult{{3}}, rows)
					return nil
				})
			}))
			require.NoError(t, run(ctx, func(c context.Context, s session) error {
				rows, err := lock.Bind(s).Find(c, first, lower)
				require.NoError(t, err)
				require.Equal(t, []lockResult{{1}}, rows)
				return nil
			}))

			// The existence shape reads only id and removed, even when the
			// data projection requires an additional column privilege.
			type textResult struct {
				Note *string `db:"note"`
			}
			notes := q.DefineView(reg, q.ViewSpec[textResult]{Name: "notes"})
			visible := plan.DefineRows(notes, "visible", src, plan.RowsSpec{WithExists: true,
				RowFilter: plan.RowFilter{Optional: []plan.RowPredicate{{Name: "item", When: plan.Eq(id.Expr(), key.Expr())}}},
			}, plan.Project(plan.Output(notes, "note", plan.Text()), note.Expr()))
			require.NoError(t, run(ctx, func(c context.Context, s session) error {
				b := visible.Bind(s)
				found, err := b.Exists(c, nil)
				require.NoError(t, err)
				require.True(t, found)
				for _, tc := range []struct {
					id   int64
					want bool
				}{{1, true}, {2, false}, {5, false}} {
					found, err := b.Exists(c, []string{"item"}, plan.BindValue(key, tc.id))
					require.NoError(t, err)
					require.Equal(t, tc.want, found)
				}
				return nil
			}))
			err = run(ctx, func(c context.Context, s session) error {
				_, err := visible.Bind(s).Find(c, plan.RowChoice{})
				return err
			})
			require.Equal(t, "42501", q.SQLState(err))

			_, err = db.ExecContext(ctx, "GRANT SELECT(note) ON "+f.table+" TO "+f.role)
			require.NoError(t, err)
			// A nullable value retains SQL NULL through the bind codec.
			text := plan.Input("note", q.CatalogType[string]("pg_catalog", "text", func(s string) any {
				if s == "" {
					return nil
				}
				return s
			}))
			compare := plan.DefineRows(view, "compare", src, plan.RowsSpec{WithExists: true,
				RowFilter: plan.RowFilter{
					Where: plan.Eq(id.Expr(), key.Expr()),
					Optional: []plan.RowPredicate{{Name: "equal", When: plan.IsNotDistinct(note.Expr(), text.Expr())},
						{Name: "different", When: plan.IsDistinct(note.Expr(), text.Expr())}},
				},
			}, projection)
			require.NoError(t, run(ctx, func(c context.Context, s session) error {
				for _, tc := range []struct {
					id    int64
					note  string
					equal bool
				}{
					{1, "", true}, {1, "ready", false}, {3, "", false}, {3, "ready", true}, {3, "other", false},
				} {
					args := []plan.Binding{plan.BindValue(key, tc.id), plan.BindValue(text, tc.note)}
					match, err := compare.Bind(s).Exists(c, []string{"equal"}, args...)
					require.NoError(t, err)
					require.Equal(t, tc.equal, match)
					different, err := compare.Bind(s).Exists(c, []string{"different"}, args...)
					require.NoError(t, err)
					require.Equal(t, !tc.equal, different)
				}
				return nil
			}))
		})
	}
}
