//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

type planItem struct {
	ID      int64     `db:"id,key"`
	Scope   itemScope `db:"scope_id,scope"`
	Rank    int32     `db:"rank"`
	Label   *string   `db:"label"`
	Version int32     `db:"revision,version"`
	Removed bool      `db:"removed,deleted"`
}

type planResult struct {
	ID    int64   `db:"id"`
	Label *string `db:"label"`
}

type planTotals struct {
	Count int64   `db:"n"`
	Min   *int32  `db:"minimum"`
	Max   *int32  `db:"maximum"`
	Sum   *int64  `db:"total"`
	Avg   *string `db:"mean"`
}

type planGroup struct {
	Label *string `db:"label"`
	Count int64   `db:"n"`
}

type rowTracer struct{ keys []string }

func (r *rowTracer) Start(ctx context.Context, info tabula.TraceInfo) (context.Context, func(int64, error)) {
	r.keys = append(r.keys, info.Key)
	return ctx, func(int64, error) {}
}

func TestRegisteredRowPlans(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx := t.Context()
			ns := fmt.Sprintf("row_plans_%d", time.Now().UnixNano())
			table := ns + ".items"
			reader, editor, remover := ns+"_reader", ns+"_editor", ns+"_remover"
			exec := func(sql string) {
				t.Helper()
				_, err := db.ExecContext(ctx, sql)
				require.NoError(t, err)
			}
			for _, role := range []string{reader, editor, remover} {
				exec("CREATE ROLE " + role + " NOBYPASSRLS")
				t.Cleanup(func() { _, err := db.Exec("DROP ROLE " + role); require.NoError(t, err) })
			}
			exec("CREATE SCHEMA " + ns)
			t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + ns + " CASCADE"); require.NoError(t, err) })
			exec("CREATE TABLE " + table + " (id bigint PRIMARY KEY, scope_id bigint NOT NULL, rank integer NOT NULL, label text, revision integer NOT NULL, removed boolean NOT NULL)")
			exec("INSERT INTO " + table + " VALUES (1,1,10,'a',0,false),(2,1,10,'a',0,false),(3,1,30,NULL,0,false),(4,2,40,'b',0,false),(5,1,50,'a',0,true),(6,1,10,NULL,0,false)")
			exec("GRANT USAGE ON SCHEMA " + ns + " TO " + strings.Join([]string{reader, editor, remover}, ", "))
			exec("GRANT SELECT(id,scope_id,rank,label,removed) ON " + table + " TO " + reader)
			exec("GRANT SELECT(id,scope_id,rank,revision,removed),UPDATE(rank,revision,removed) ON " + table + " TO " + editor)
			exec("GRANT SELECT(id,scope_id,revision,removed),DELETE ON " + table + " TO " + remover)

			trace := new(rowTracer)
			reg := tabula.NewRegistry(tabula.RegistryConfig[itemScope]{Deny: tabula.DenyLegacySQL, Tracer: trace, ScopeID: func(s itemScope) any { return int64(s) }})
			tab := tabula.Define(reg, tabula.TableSpec[planItem, int64]{Name: table, AllowDelete: true})
			rel := tabula.FromTable(tab, "i", tabula.ByScopeColumn())
			scope := tabula.FieldOf(rel, "scope_id", tabula.CatalogType[itemScope]("pg_catalog", "int8", nil))
			src := tabula.RowsFrom(tab, rel, scope.Column())
			id, rank := tabula.FieldOf(rel, "id", tabula.Int8()), tabula.FieldOf(rel, "rank", tabula.Int4())
			label, revision := tabula.FieldOf(rel, "label", tabula.Text()), tabula.FieldOf(rel, "revision", tabula.Int4())
			removed := tabula.FieldOf(rel, "removed", tabula.Bool())
			minimum := tabula.Input("minimum", tabula.Int8())
			minRank, newRank := tabula.Input("min_rank", tabula.Int4()), tabula.Input("new_rank", tabula.Int4())
			expected, remove := tabula.Input("previous", tabula.Int4()), tabula.Input("remove", tabula.Bool())
			out := tabula.DefineView(reg, tabula.ViewSpec[planResult]{Name: "entries"})
			quiet := []tabula.StatementOption{tabula.Quiet()}
			filter := tabula.RowFilter{Observe: quiet, Optional: []tabula.RowPredicate{{Name: "minimum", When: tabula.Ge(id.Expr(), minimum.Expr())}}}
			search := tabula.DefineRows(out, "search", src, tabula.RowsSpec{RowFilter: filter,
				Orders: []tabula.RowOrder{{Name: "forward", Terms: []tabula.RowTerm{tabula.Asc(label), tabula.Desc(id)}}, {Name: "reverse", Terms: []tabula.RowTerm{tabula.Desc(label), tabula.Asc(id)}}},
				Pages:  tabula.RowPages{Unpaged: true, Offset: true, Keyset: true, MaxLimit: 10, MaxOffset: 12}},
				tabula.Project(tabula.Output(out, "id", tabula.Int8()), id.Expr()), tabula.Project(tabula.Output(out, "label", tabula.Text()), label.Expr()))
			totals := tabula.DefineView(reg, tabula.ViewSpec[planTotals]{Name: "notes"})
			numeric := tabula.CatalogType[string]("pg_catalog", "numeric", nil)
			scalar := tabula.DefineRows(totals, "totals", src, tabula.RowsSpec{RowFilter: filter},
				tabula.Project(tabula.Output(totals, "n", tabula.Int8()), tabula.CountAll()),
				tabula.Project(tabula.Output(totals, "minimum", tabula.Int4()), tabula.MinOf(rank.Expr())),
				tabula.Project(tabula.Output(totals, "maximum", tabula.Int4()), tabula.MaxOf(rank.Expr())),
				tabula.Project(tabula.Output(totals, "total", tabula.Int8()), tabula.SumOf(rank.Expr(), tabula.Int8())),
				tabula.Project(tabula.Output(totals, "mean", numeric), tabula.AvgOf(rank.Expr(), numeric)))
			groups := tabula.DefineView(reg, tabula.ViewSpec[planGroup]{Name: "groups"})
			grouped := tabula.DefineRows(groups, "counts", src, tabula.RowsSpec{RowFilter: filter, GroupBy: []tabula.RowColumn{label.Column()},
				Orders: []tabula.RowOrder{{Name: "label", Terms: []tabula.RowTerm{tabula.Asc(label)}}}, Pages: tabula.RowPages{Offset: true, MaxLimit: 3, MaxOffset: 10}},
				tabula.Project(tabula.Output(groups, "label", tabula.Text()), label.Expr()), tabula.Project(tabula.Output(groups, "n", tabula.Int8()), tabula.CountOf(id.Expr())))
			base := tabula.MutationSpec{RowFilter: tabula.RowFilter{Observe: quiet, Where: tabula.Ge(id.Expr(), minimum.Expr())}, Version: tabula.ExpectVersion(revision, expected)}
			del := tabula.DefineDeleteMany(src, "remove_live", base)
			base.Optional = []tabula.RowPredicate{{Name: "rank", When: tabula.Ge(rank.Expr(), minRank.Expr())}}
			mod := tabula.DefineUpdateMany(src, "change", base,
				tabula.RowSet{Name: "priority", Assignments: []tabula.RowAssignment{tabula.Assign(rank, newRank.Expr())}},
				tabula.RowSet{Name: "hide", Assignments: []tabula.RowAssignment{tabula.Assign(removed, remove.Expr())}})

			// Bind each family from the serialized snapshot, with the same names
			// that CollectSQL emits for the constructors.
			snap := sqlcheck.RenderAllSnapshot(reg.Defined(), reg.DefinedViews())
			defs, err := sqlcheck.ParseSnapshot(snap)
			require.NoError(t, err)
			require.Equal(t, snap, sqlcheck.RenderSnapshot(defs))
			inv := sqlcheck.SQLInventory{}
			bindings := map[string]sqlcheck.SQLBinding{}
			for _, f := range []struct{ owner, plan, method, role string }{
				{"entries", "search", "DefineRows", reader}, {"notes", "totals", "DefineRows", reader}, {"groups", "counts", "DefineRows", reader},
				{table, "change", "DefineUpdateMany", editor}, {table, "remove_live", "DefineDeleteMany", remover},
			} {
				inv.Sites = append(inv.Sites, sqlcheck.SQLSite{ID: f.plan, Method: f.method, Kind: "typed_rows", Definition: f.owner, QueryKey: f.plan})
				bindings[f.plan] = sqlcheck.SQLBinding{Roles: []string{f.role}}
			}
			work, err := inv.BindRolesWithDefinitions(bindings, defs, nil)
			require.NoError(t, err)
			require.Len(t, work[reader], 2*(1+2*4)+2*2+2*2)
			require.Len(t, work[editor], 4)
			require.Len(t, work[remover], 1)
			for role, stmts := range work {
				for _, stmt := range stmts {
					require.NotEmpty(t, stmt.Access)
					needs, err := (sqlcheck.RoleStatements{role: {stmt}}).Readiness(role)
					require.NoError(t, err)
					missing, err := sqlcheck.CheckReadiness(ctx, db, role, needs)
					require.NoError(t, err)
					require.Empty(t, missing, "%s/%s", role, stmt.Name)
				}
			}
			findings, err := sqlcheck.VerifyRoleStatements(ctx, db, work)
			require.NoError(t, err)
			require.Empty(t, findings)
			findings, err = sqlcheck.CheckPolicyCoverage(ctx, db, work)
			require.NoError(t, err)
			require.Empty(t, findings)
			exec("REVOKE UPDATE(rank) ON " + table + " FROM " + editor)
			findings, err = sqlcheck.VerifyRoleStatements(ctx, db, sqlcheck.RoleStatements{editor: work[editor]})
			require.NoError(t, err)
			require.Len(t, findings, 2)
			for _, failure := range findings {
				require.Contains(t, failure, "set.priority")
			}
			exec("GRANT UPDATE(rank) ON " + table + " TO " + editor)
			var logBytes bytes.Buffer
			log := tabula.LogConfig{Level: tabula.LogAll, Logger: slog.New(slog.NewJSONHandler(&logBytes, nil))}
			run := func(role string, scope itemScope, body rowCallback) {
				t.Helper()
				require.NoError(t, runRowPlan(t, db, pool, driver, reg.SQLAuthority(), role, scope, log, body))
			}
			run(reader, 1, func(ctx context.Context, s tabula.Session[itemScope]) error {
				bound := search.Bind(s)
				for _, order := range []string{"forward", "reverse"} {
					all, total, err := bound.List(ctx, tabula.RowChoice{Order: order})
					require.NoError(t, err)
					require.EqualValues(t, 4, total)
					require.Len(t, all, 4)
					for i, row := range all {
						c := tabula.RowChoice{Order: order, Page: tabula.FirstPage, Limit: 1}
						if i != 0 {
							prev := all[i-1]
							pos := tabula.NullAt(label)
							if prev.Label != nil {
								pos = tabula.At(label, *prev.Label)
							}
							c.After, err = search.Cursor(order, pos, tabula.At(id, prev.ID))
							require.NoError(t, err)
							c.Page = tabula.AfterPage
						}
						page, total, err := bound.List(ctx, c)
						require.NoError(t, err)
						require.EqualValues(t, 4, total)
						require.Equal(t, []planResult{row}, page)
					}
				}
				b := scalar.Bind(s)
				rows, n, err := b.List(ctx, tabula.RowChoice{})
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				require.Len(t, rows, 1)
				require.EqualValues(t, 4, rows[0].Count)
				require.EqualValues(t, 60, *rows[0].Sum)
				require.EqualValues(t, 10, *rows[0].Min)
				require.EqualValues(t, 30, *rows[0].Max)
				require.Contains(t, *rows[0].Avg, "15")
				rows, n, err = b.List(ctx, tabula.RowChoice{Filters: []string{"minimum"}}, tabula.BindValue(minimum, 100))
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				require.Equal(t, []planTotals{{}}, rows)
				group, n, err := grouped.Bind(s).List(ctx, tabula.RowChoice{Order: "label", Page: tabula.OffsetPage, Limit: 1, Offset: 1})
				require.NoError(t, err)
				require.EqualValues(t, 2, n)
				require.Equal(t, []planGroup{{Count: 2}}, group)
				return nil
			})
			run(editor, 1, func(ctx context.Context, s tabula.Session[itemScope]) error {
				b := mod.Bind(s)
				update := func(id int64, version, rank int32, want int64) {
					n, err := b.UpdateMany(ctx, "priority", nil, tabula.BindValue(expected, version), tabula.BindValue(minimum, id), tabula.BindValue(newRank, rank))
					require.NoError(t, err)
					require.Equal(t, want, n)
				}
				update(100, 0, 9, 0)
				update(6, 0, 9, 1)
				update(1, 0, 9, 3)
				update(1, 0, 8, 0)
				n, err := b.UpdateMany(ctx, "hide", []string{"rank"}, tabula.BindValue(expected, 1), tabula.BindValue(minimum, 6), tabula.BindValue(minRank, 9), tabula.BindValue(remove, true))
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				return nil
			})
			run(remover, 1, func(ctx context.Context, s tabula.Session[itemScope]) error {
				b := del.Bind(s)
				for _, tc := range []struct{ minimum, want int64 }{{6, 0}, {3, 1}, {1, 2}} {
					n, err := b.DeleteMany(ctx, nil, tabula.BindValue(minimum, tc.minimum), tabula.BindValue(expected, 1))
					require.NoError(t, err)
					require.Equal(t, tc.want, n)
				}
				return nil
			})
			run(reader, 2, func(ctx context.Context, s tabula.Session[itemScope]) error {
				rows, n, err := search.Bind(s).List(ctx, tabula.RowChoice{Order: "forward", Page: tabula.OffsetPage, Limit: 5})
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				require.EqualValues(t, 4, rows[0].ID)
				return nil
			})
			require.Empty(t, logBytes.String())
			err = runRowPlan(t, db, pool, driver, reg.SQLAuthority(), reader, 1, log, func(ctx context.Context, s tabula.Session[itemScope]) error {
				_, err := mod.Bind(s).UpdateMany(ctx, "priority", nil, tabula.BindValue(expected, 0), tabula.BindValue(minimum, 1), tabula.BindValue(newRank, 4))
				return err
			})
			require.Error(t, err)
			require.Contains(t, logBytes.String(), "tabula.statement_failed")
			require.Contains(t, logBytes.String(), table+".change.scoped.set.priority")
			var remaining int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&remaining))
			require.Equal(t, 3, remaining)
			require.Contains(t, trace.keys, "entries.search.scoped.count")
			require.Contains(t, trace.keys, table+".change.scoped.set.priority")
			require.True(t, slices.Contains(trace.keys, "entries.search.scoped.data.sort.forward.after"))

			// A database-enforced relation uses the runner's role and setting
			// for scope. Its typed shapes reserve no application scope slot.
			exec("UPDATE " + table + " SET removed=false WHERE id=5")
			exec("ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY")
			exec("ALTER TABLE " + table + " FORCE ROW LEVEL SECURITY")
			exec("CREATE POLICY visible ON " + table + " FOR SELECT TO " + reader + " USING (scope_id=NULLIF(current_setting('demo.scope',true),'')::bigint)")
			rlsTable := tabula.Define(reg, tabula.TableSpec[planItem, int64]{Name: table, ReadOnly: true, RelationScope: tabula.ByRLS()})
			rs := tabula.FromTable(rlsTable, "r", tabula.ByRLS())
			read := tabula.DefineRows(out, "visible", tabula.RowsFrom(rlsTable, rs), tabula.RowsSpec{WithExists: true},
				tabula.Project(tabula.Output(out, "id", tabula.Int8()), tabula.FieldOf(rs, "id", tabula.Int8()).Expr()),
				tabula.Project(tabula.Output(out, "label", tabula.Text()), tabula.FieldOf(rs, "label", tabula.Text()).Expr()))
			inv = sqlcheck.SQLInventory{Sites: []sqlcheck.SQLSite{{ID: "rls", Kind: "typed_rows", Method: "DefineRows", Definition: "entries", QueryKey: "visible"}}}
			rlsWork, err := inv.BindRolesWithDefinitions(map[string]sqlcheck.SQLBinding{"rls": {Roles: []string{reader}}}, nil, reg.DefinedViews())
			require.NoError(t, err)
			for _, stmt := range rlsWork[reader] {
				require.NotContains(t, stmt.SQL, "scope_id")
				require.Empty(t, stmt.Shape.Inputs)
			}
			rlsNeeds, err := rlsWork.Readiness(reader)
			require.NoError(t, err)
			verify := func() []sqlcheck.MissingRequirement {
				missing, err := sqlcheck.CheckReadiness(ctx, db, reader, rlsNeeds)
				require.NoError(t, err)
				return missing
			}
			require.Empty(t, verify())
			findings, err = sqlcheck.VerifyRoleStatements(ctx, db, rlsWork)
			require.NoError(t, err)
			require.Empty(t, findings)
			findings, err = sqlcheck.CheckPolicyCoverage(ctx, db, rlsWork)
			require.NoError(t, err)
			require.Empty(t, findings)
			for _, tc := range []struct {
				scope itemScope
				id    int64
			}{{1, 5}, {2, 4}} {
				run(reader, tc.scope, func(ctx context.Context, s tabula.Session[itemScope]) error {
					rows, count, err := read.Bind(s).List(ctx, tabula.RowChoice{})
					require.NoError(t, err)
					require.EqualValues(t, 1, count)
					require.Equal(t, tc.id, rows[0].ID)
					found, err := read.Bind(s).Exists(ctx, nil)
					require.NoError(t, err)
					require.True(t, found)
					return nil
				})
			}
			run(reader, 3, func(ctx context.Context, s tabula.Session[itemScope]) error {
				found, err := read.Bind(s).Exists(ctx, nil)
				require.NoError(t, err)
				require.False(t, found)
				return nil
			})
			exec("ALTER TABLE " + table + " NO FORCE ROW LEVEL SECURITY")
			missing := verify()
			require.Len(t, missing, 1)
			require.Equal(t, "FORCE RLS", missing[0].Privilege)
		})
	}
}
