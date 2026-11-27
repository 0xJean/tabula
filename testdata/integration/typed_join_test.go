//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

type joinGroup struct {
	ID      int64     `db:"id,key"`
	Scope   itemScope `db:"scope_id,scope"`
	Name    string    `db:"name"`
	Removed bool      `db:"removed,deleted"`
}

type joinJob struct {
	ID      int64     `db:"id,key"`
	Scope   itemScope `db:"scope_id,scope"`
	GroupID int64     `db:"group_id"`
	Removed bool      `db:"removed,deleted"`
}

type joinPair struct {
	Group int64  `db:"group_id"`
	Job   *int64 `db:"job_id"`
}

type groupTotal struct {
	Group int64 `db:"group_id"`
	N     int64 `db:"n"`
}

func prepareJoinedTables(t *testing.T, db *sql.DB) (schema, role string) {
	t.Helper()
	schema = fmt.Sprintf("joined_%d", time.Now().UnixNano())
	role = schema + "_reader"
	exec := func(query string) {
		t.Helper()
		_, err := db.ExecContext(t.Context(), query)
		require.NoError(t, err)
	}
	exec("CREATE ROLE " + role + " NOBYPASSRLS")
	t.Cleanup(func() { _, err := db.Exec("DROP ROLE " + role); require.NoError(t, err) })
	exec("CREATE SCHEMA " + schema)
	t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + schema + " CASCADE"); require.NoError(t, err) })
	groups, jobs := schema+".groups", schema+".jobs"
	exec("CREATE TABLE " + groups + "(id bigint PRIMARY KEY, scope_id bigint NOT NULL, name text NOT NULL, removed bool NOT NULL)")
	exec("CREATE TABLE " + jobs + "(id bigint PRIMARY KEY, scope_id bigint NOT NULL, group_id bigint NOT NULL, removed bool NOT NULL)")
	exec("INSERT INTO " + groups + " VALUES (1,1,'Alpha',false),(2,1,'Beta',false),(3,1,'Empty',false),(4,2,'Other',false),(5,1,'Old',true)")
	exec("INSERT INTO " + jobs + " VALUES (10,1,1,false),(11,2,2,false),(12,1,2,true),(13,2,4,false)")
	exec("GRANT USAGE ON SCHEMA " + schema + " TO " + role)
	exec("GRANT SELECT(id,scope_id,name,removed) ON " + groups + " TO " + role)
	exec("GRANT SELECT(id,scope_id,group_id,removed) ON " + jobs + " TO " + role)
	return schema, role
}

func TestExplicitJoinDrivers(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ns, role := prepareJoinedTables(t, db)
			cfg := q.RegistryConfig[itemScope]{Deny: q.DenyLegacySQL}
			if driver == "stdsql" {
				cfg.ArrayArg = pqdriver.Array
			}
			reg := q.NewRegistry(cfg)
			groups := q.Define(reg, q.TableSpec[joinGroup, int64]{Name: ns + ".groups"})
			jobs := q.Define(reg, q.TableSpec[joinJob, int64]{Name: ns + ".jobs"})
			g, j := plan.FromTable(groups, "g", q.ByScopeColumn()), plan.FromTable(jobs, "j", q.ByScopeColumn())
			scope := q.CatalogType[itemScope]("pg_catalog", "int8", nil)
			left := plan.RowsFrom(groups, g, plan.FieldOf(g, "scope_id", scope).Column())
			right := plan.RowsFrom(jobs, j, plan.FieldOf(j, "scope_id", scope).Column())
			gid, jid := plan.FieldOf(g, "id", plan.Int8()), plan.FieldOf(j, "id", plan.Int8())
			name := plan.FieldOf(g, "name", plan.Text())
			on := plan.Eq(gid.Expr(), plan.FieldOf(j, "group_id", plan.Int8()).Expr())
			out := q.DefineView(reg, q.ViewSpec[joinPair]{Name: "entries"})
			base := plan.Select(plan.Project(plan.Output(out, "group_id", plan.Int8()), gid.Expr()), plan.Project(plan.Output(out, "job_id", plan.Int8()), jid.Expr())).From(left)
			pairs := plan.DefineRead(out, "pairs", base.LeftJoin(right, on).OrderBy(plan.Asc(gid)))
			matches := plan.DefineRead(out, "matches", base.InnerJoin(right, on))
			product := plan.DefineRead(out, "pairs_product", base.CrossJoin(right).OrderBy(plan.Asc(gid)))
			summary := q.DefineView(reg, q.ViewSpec[groupTotal]{Name: "notes"})
			minimum := plan.Input("minimum", plan.Int8())
			count := plan.CountOf(jid.Expr())
			totals := plan.DefineRead(summary, "summary", plan.Select(plan.Project(plan.Output(summary, "group_id", plan.Int8()), gid.Expr()),
				plan.Project(plan.Output(summary, "n", plan.Int8()), count)).From(left).LeftJoin(right, on).
				GroupBy(gid.Expr()).Having(plan.Ge(count, minimum.Expr())).OrderBy(plan.Asc(gid)))

			keys := plan.Input("keys", q.ArrayOf(plan.Int8()))
			a, b := plan.Input("a", plan.Int8()), plan.Input("b", plan.Int8())
			pattern, folded := plan.Input("pattern", plan.Text()), plan.Input("folded", plan.Text())
			filter := plan.And(plan.Any(gid.Expr(), keys.Expr()), plan.In(gid.Expr(), a.Expr(), b.Expr()), plan.Like(name.Expr(), pattern.Expr()), plan.ILike(name.Expr(), folded.Expr()))
			filtered := plan.DefineRead(out, "filtered", base.InnerJoin(right, on).Where(filter))
			work := sqlcheck.RoleStatements{}
			work.AddDefinitions(role, sqlcheck.ViewsToDefinitions(reg.DefinedViews())...)
			needs, err := work.Readiness(role)
			require.NoError(t, err)
			missing, err := sqlcheck.CheckReadiness(t.Context(), db, role, needs)
			require.NoError(t, err)
			require.Empty(t, missing)
			findings, err := sqlcheck.VerifyRoleStatements(t.Context(), db, work)
			require.NoError(t, err)
			require.Empty(t, findings)
			run := func(scope itemScope, body rowCallback) error {
				return runRowPlan(t, db, pool, driver, reg.SQLAuthority(), role, scope, q.LogConfig{}, body)
			}
			require.NoError(t, run(1, func(ctx context.Context, s q.Session[itemScope]) error {
				rows, err := pairs.Bind(s).Find(ctx)
				require.NoError(t, err)
				require.Equal(t, []joinPair{{1, new(int64(10))}, {2, nil}, {3, nil}}, rows)
				rows, err = matches.Bind(s).Find(ctx)
				require.NoError(t, err)
				require.Equal(t, []joinPair{{1, new(int64(10))}}, rows)
				rows, err = product.Bind(s).Find(ctx)
				require.NoError(t, err)
				require.Equal(t, []joinPair{{1, new(int64(10))}, {2, new(int64(10))}, {3, new(int64(10))}}, rows)
				for _, tc := range []struct {
					n    int64
					want []groupTotal
				}{{0, []groupTotal{{1, 1}, {2, 0}, {3, 0}}}, {1, []groupTotal{{1, 1}}}} {
					got, err := totals.Bind(s).Find(ctx, plan.BindValue(minimum, tc.n))
					require.NoError(t, err)
					require.Equal(t, tc.want, got)
				}
				for _, tc := range []struct {
					keys    []int64
					pattern string
					n       int
				}{{[]int64{1, 2}, "Al_h%", 1}, {[]int64{1}, "al%", 0}, {[]int64{}, "%", 0}, {nil, "%", 0}} {
					rows, err = filtered.Bind(s).Find(ctx, plan.BindValue(keys, tc.keys), plan.BindValue(a, 1), plan.BindValue(b, 2),
						plan.BindValue(pattern, tc.pattern), plan.BindValue(folded, "aL%"))
					require.NoError(t, err)
					require.Len(t, rows, tc.n)
				}
				return nil
			}))
			require.NoError(t, run(2, func(ctx context.Context, s q.Session[itemScope]) error {
				rows, err := pairs.Bind(s).Find(ctx)
				require.Equal(t, []joinPair{{4, new(int64(13))}}, rows)
				return err
			}))
			_, err = db.ExecContext(t.Context(), "REVOKE SELECT(group_id) ON "+ns+".jobs FROM "+role)
			require.NoError(t, err)
			missing, err = sqlcheck.CheckReadiness(t.Context(), db, role, needs)
			require.NoError(t, err)
			require.Len(t, missing, 1)
			err = run(1, func(ctx context.Context, s q.Session[itemScope]) error {
				_, err := pairs.Bind(s).Find(ctx)
				return err
			})
			require.Equal(t, "42501", q.SQLState(err))
		})
	}
}

func TestExplicitJoinWithRLS(t *testing.T) {
	db, pool := databases(t)
	ns, role := prepareJoinedTables(t, db)
	exec := func(query string) {
		t.Helper()
		_, err := db.ExecContext(t.Context(), query)
		require.NoError(t, err)
	}
	for _, name := range []string{"groups", "jobs"} {
		table := ns + "." + name
		exec("ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY")
		exec("ALTER TABLE " + table + " FORCE ROW LEVEL SECURITY")
		exec("CREATE POLICY visible ON " + table + " FOR SELECT TO " + role + " USING (scope_id = current_setting('demo.scope',true)::bigint)")
	}
	reg := q.NewRegistry(q.RegistryConfig[itemScope]{Deny: q.DenyLegacySQL})
	groups := q.Define(reg, q.TableSpec[joinGroup, int64]{Name: ns + ".groups", RelationScope: q.ByRLS()})
	jobs := q.Define(reg, q.TableSpec[joinJob, int64]{Name: ns + ".jobs", RelationScope: q.ByRLS()})
	g, j := plan.FromTable(groups, "g", q.ByRLS()), plan.FromTable(jobs, "j", q.ByRLS())
	gid := plan.FieldOf(g, "id", plan.Int8())
	out := q.DefineView(reg, q.ViewSpec[joinPair]{Name: "entries"})
	p := plan.DefineRead(out, "pairs", plan.Select(plan.Project(plan.Output(out, "group_id", plan.Int8()), gid.Expr()),
		plan.Project(plan.Output(out, "job_id", plan.Int8()), plan.FieldOf(j, "id", plan.Int8()).Expr())).From(g).
		LeftJoin(j, plan.Eq(gid.Expr(), plan.FieldOf(j, "group_id", plan.Int8()).Expr())).OrderBy(plan.Asc(gid)))
	peer := plan.FromTable(groups, "peer", q.ByRLS())
	peerID := plan.FieldOf(peer, "id", plan.Int8())
	self := plan.DefineRead(out, "peers", plan.Select(plan.Project(plan.Output(out, "group_id", plan.Int8()), gid.Expr()),
		plan.Project(plan.Output(out, "job_id", plan.Int8()), peerID.Expr())).From(g).
		InnerJoin(peer, plan.Eq(gid.Expr(), peerID.Expr())).OrderBy(plan.Asc(gid)))
	def := out.Definition()
	require.Empty(t, def.StatementShapes["pairs.rls.data"].Inputs)
	require.NotContains(t, def.Statements["pairs.rls.data"], "scope_id")
	work := sqlcheck.RoleStatements{}
	work.AddDefinitions(role, sqlcheck.ViewsToDefinitions(reg.DefinedViews())...)
	needs, err := work.Readiness(role)
	require.NoError(t, err)
	require.Equal(t, []string{`"` + ns + `"."groups"`, `"` + ns + `"."jobs"`}, needs.ForceRLS)
	missing, err := sqlcheck.CheckReadiness(t.Context(), db, role, needs)
	require.NoError(t, err)
	require.Empty(t, missing)
	findings, err := sqlcheck.VerifyRoleStatements(t.Context(), db, work)
	require.NoError(t, err)
	require.Empty(t, findings)
	for _, driver := range []string{"stdsql", "pgx"} {
		for _, scope := range []itemScope{1, 2} {
			err = runRowPlan(t, db, pool, driver, reg.SQLAuthority(), role, scope, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				rows, err := p.Bind(s).Find(ctx)
				require.NoError(t, err)
				if scope == 1 {
					require.Equal(t, []joinPair{{1, new(int64(10))}, {2, nil}, {3, nil}}, rows)
				} else {
					require.Equal(t, []joinPair{{4, new(int64(13))}}, rows)
				}
				peers, err := self.Bind(s).Find(ctx)
				require.NoError(t, err)
				require.Len(t, peers, len(rows))
				for i, match := range peers {
					require.Equal(t, rows[i].Group, match.Group)
					require.Equal(t, new(match.Group), match.Job)
				}
				return nil
			})
			require.NoError(t, err)
		}
	}
	exec("ALTER TABLE " + ns + ".jobs NO FORCE ROW LEVEL SECURITY")
	missing, err = sqlcheck.CheckReadiness(t.Context(), db, role, needs)
	require.NoError(t, err)
	require.Len(t, missing, 1)
	require.Contains(t, missing[0].Object, "jobs")
}
