//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/stretchr/testify/require"
)

func TestQuerySetNullMultiplicityDrivers(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			cfg := q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL}
			if driver == "pq" {
				cfg.ArrayArg = pqdriver.Array
			}
			reg := q.NewRegistry(cfg)
			out := q.DefineView(reg, q.ViewSpec[composedValue]{Name: "entries"})
			target := q.Output(out, "value", q.Int8())
			left, right := q.Input("values", q.ArrayOfNullable(q.Int8())), q.Input("values", q.ArrayOfNullable(q.Int8()))
			l, r := q.UnnestColumn("value", left.Expr(), q.Int8()), q.UnnestColumn("value", right.Expr(), q.Int8())
			ls, rs := q.Unnest("u", l), q.Unnest("u", r)
			lf, rf := q.Export("value", l.From(ls)), q.Export("value", r.From(rs))
			a, b := q.Select(lf.Project()).From(ls), q.Select(rf.Project()).From(rs)
			for _, test := range []struct {
				name  string
				build func(q.ReadSelect, q.ReadSelect, ...q.ReadSelect) q.ReadSelect
				want  []composedValue
			}{
				{"union", q.Union, []composedValue{{new(int64(1))}, {new(int64(2))}, {nil}}},
				{"union_all", q.UnionAll, []composedValue{{new(int64(1))}, {new(int64(2))}, {nil}, {nil}, {nil}}},
				{"intersect", q.Intersect, []composedValue{{nil}}},
				{"intersect_all", q.IntersectAll, []composedValue{{nil}}},
				{"except", q.Except, []composedValue{{new(int64(1))}}},
				{"except_all", q.ExceptAll, []composedValue{{new(int64(1))}, {nil}}},
			} {
				d := q.Derived("d", test.build(a, b))
				plan := q.DefineRead(out, test.name, q.Select(q.Project(target, lf.From(d))).From(d).OrderBy(q.AscExpr(lf.From(d)).NullsLast()))
				run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
				require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
					rows, err := plan.Bind(s).Find(ctx, q.BindValue(left, []*int64{nil, nil, new(int64(1))}), q.BindValue(right, []*int64{nil, new(int64(2))}))
					require.NoError(t, err)
					require.Equal(t, test.want, rows)
					return nil
				}))
			}
			d := q.Derived("d", q.IntersectAll(q.UnionAll(a.OrderBy(q.DescExpr(l.From(ls))).Take(2), b.Take(1)), b))
			plan := q.DefineRead(out, "nested", q.Select(q.Project(target, lf.From(d))).From(d).OrderBy(q.AscExpr(lf.From(d))))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				rows, err := plan.Bind(s).Find(ctx, q.BindValue(left, []*int64{new(int64(3)), new(int64(1)), new(int64(2))}), q.BindValue(right, []*int64{new(int64(4)), new(int64(2))}))
				require.NoError(t, err)
				require.Equal(t, []composedValue{{new(int64(2))}, {new(int64(4))}}, rows)
				return nil
			}))
		})
	}
}

func TestQueryCommonTableSelfJoinDrivers(t *testing.T) {
	type pair struct {
		First  int64 `db:"first"`
		Second int64 `db:"second"`
	}
	db, pool := databases(t)
	schema := typesSchema(t, db)
	_, err := db.ExecContext(t.Context(), "CREATE TABLE "+schema+".items(id bigint PRIMARY KEY,value bigint); INSERT INTO "+schema+".items VALUES (1,10),(2,20),(3,NULL)")
	require.NoError(t, err)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			table := q.DefinePlanSource(reg, q.TableSpec[composedItem, int64]{Name: schema + ".items", RelationScope: q.Global()})
			src := q.MapTable(table).As("i")
			value := q.Export("value", q.FieldOf(src.Rel(), "id", q.Int8()).Expr())
			out := q.DefineView(reg, q.ViewSpec[pair]{Name: "entries"})
			bound := q.Input("bound", q.Int8())
			for _, test := range []struct {
				name string
				mode q.Materialization
			}{{"default", q.DefaultMaterialization}, {"stored", q.Materialized}, {"inline", q.NotMaterialized}} {
				first := q.CTE("notes", q.Select(value.Project()).From(src), test.mode)
				f := first.As("f")
				next := q.Export("next", value.From(f))
				second := q.CTE("groups", q.Select(next.Project()).From(f).Where(q.Le(value.From(f), bound.Expr())), test.mode)
				l, r := second.As("l"), second.As("r")
				plan := q.DefineRead(out, test.name, q.Select(q.Project(q.Output(out, "first", q.Int8()), next.From(r)), q.Project(q.Output(out, "second", q.Int8()), next.From(l))).From(l).InnerJoin(r, q.Eq(next.From(l), next.From(r))).With(first, second).OrderBy(q.AscExpr(next.From(l))))
				run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
				require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
					rows, err := plan.Bind(s).Find(ctx, q.BindValue(bound, int64(2)))
					require.NoError(t, err)
					require.Equal(t, []pair{{1, 1}, {2, 2}}, rows)
					return nil
				}))
			}
		})
	}
}

func TestQueryRecursiveScopeDrivers(t *testing.T) {
	type item struct {
		ID    int64     `db:"id,key"`
		Scope itemScope `db:"scope_id,scope"`
	}
	db, pool := databases(t)
	schema := typesSchema(t, db)
	_, err := db.ExecContext(t.Context(), "CREATE TABLE "+schema+".items(id bigint PRIMARY KEY,scope_id bigint NOT NULL); INSERT INTO "+schema+".items VALUES(1,1),(2,2),(3,1)")
	require.NoError(t, err)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			reg := q.NewRegistry(q.RegistryConfig[itemScope]{Deny: q.DenyLegacySQL, DefaultTypes: []q.TypeDefault{q.TypeMapping(q.CatalogType[itemScope]("pg_catalog", "int8", nil))}})
			table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: schema + ".items", RelationScope: q.ByScopeColumn()})
			layout := q.MapTable(table)
			a, b := layout.As("i"), layout.As("i")
			id := q.MappedColumn[int64](layout, "id")
			first, one := q.Input("first", q.Int8()), q.Input("one", q.Int8())
			value := q.Export("value", id.On(a.Rel()).Expr())
			common := q.RecursiveCTE("notes", q.Select(value.Project()).From(a).Where(q.Eq(id.On(a.Rel()).Expr(), first.Expr())), q.RecursiveUnionDistinct, func(self q.QuerySource) q.ReadSelect {
				return q.Select(q.Export("value", id.On(b.Rel()).Expr()).Project()).From(self).InnerJoin(b, q.Eq(id.On(b.Rel()).Expr(), q.Plus(value.From(self), one.Expr())))
			})
			n := common.As("n")
			out := q.DefineView(reg, q.ViewSpec[composedValue]{Name: "entries"})
			plan := q.DefineRead(out, "scoped", q.Select(q.Project(q.Output(out, "value", q.Int8()), value.From(n))).From(n).With(common))
			for _, test := range []struct {
				scope itemScope
				first int64
				want  []composedValue
			}{{1, 1, []composedValue{{new(int64(1))}}}, {2, 2, []composedValue{{new(int64(2))}}}, {2, 1, nil}} {
				require.NoError(t, runRowPlan(t, db, pool, driver, reg.SQLAuthority(), "", test.scope, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
					rows, err := plan.Bind(s).Find(ctx, q.BindValue(first, test.first), q.BindValue(one, int64(1)))
					require.NoError(t, err)
					require.Equal(t, test.want, rows)
					return nil
				}))
			}
		})
	}
}
