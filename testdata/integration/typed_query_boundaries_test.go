//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	tplan "github.com/0xJean/tabula/plan"
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
			target := tplan.Output(out, "value", tplan.Int8())
			left, right := tplan.Input("values", q.ArrayOfNullable(tplan.Int8())), tplan.Input("values", q.ArrayOfNullable(tplan.Int8()))
			l, r := tplan.UnnestColumn("value", left.Expr(), tplan.Int8()), tplan.UnnestColumn("value", right.Expr(), tplan.Int8())
			ls, rs := tplan.Unnest("u", l), tplan.Unnest("u", r)
			lf, rf := tplan.Export("value", l.From(ls)), tplan.Export("value", r.From(rs))
			a, b := tplan.Select(lf.Project()).From(ls), tplan.Select(rf.Project()).From(rs)
			for _, test := range []struct {
				name  string
				build func(tplan.ReadSelect, tplan.ReadSelect, ...tplan.ReadSelect) tplan.ReadSelect
				want  []composedValue
			}{
				{"union", tplan.Union, []composedValue{{new(int64(1))}, {new(int64(2))}, {nil}}},
				{"union_all", tplan.UnionAll, []composedValue{{new(int64(1))}, {new(int64(2))}, {nil}, {nil}, {nil}}},
				{"intersect", tplan.Intersect, []composedValue{{nil}}},
				{"intersect_all", tplan.IntersectAll, []composedValue{{nil}}},
				{"except", tplan.Except, []composedValue{{new(int64(1))}}},
				{"except_all", tplan.ExceptAll, []composedValue{{new(int64(1))}, {nil}}},
			} {
				d := tplan.Derived("d", test.build(a, b))
				plan := tplan.DefineRead(out, test.name, tplan.Select(tplan.Project(target, lf.From(d))).From(d).OrderBy(tplan.AscExpr(lf.From(d)).NullsLast()))
				run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
				require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
					rows, err := plan.Bind(s).Find(ctx, tplan.BindValue(left, []*int64{nil, nil, new(int64(1))}), tplan.BindValue(right, []*int64{nil, new(int64(2))}))
					require.NoError(t, err)
					require.Equal(t, test.want, rows)
					return nil
				}))
			}
			d := tplan.Derived("d", tplan.IntersectAll(tplan.UnionAll(a.OrderBy(tplan.DescExpr(l.From(ls))).Take(2), b.Take(1)), b))
			plan := tplan.DefineRead(out, "nested", tplan.Select(tplan.Project(target, lf.From(d))).From(d).OrderBy(tplan.AscExpr(lf.From(d))))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				rows, err := plan.Bind(s).Find(ctx, tplan.BindValue(left, []*int64{new(int64(3)), new(int64(1)), new(int64(2))}), tplan.BindValue(right, []*int64{new(int64(4)), new(int64(2))}))
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
			src := tplan.MapTable(table).As("i")
			value := tplan.Export("value", tplan.FieldOf(src.Rel(), "id", tplan.Int8()).Expr())
			out := q.DefineView(reg, q.ViewSpec[pair]{Name: "entries"})
			bound := tplan.Input("bound", tplan.Int8())
			for _, test := range []struct {
				name string
				mode tplan.Materialization
			}{{"default", tplan.DefaultMaterialization}, {"stored", tplan.Materialized}, {"inline", tplan.NotMaterialized}} {
				first := tplan.CTE("notes", tplan.Select(value.Project()).From(src), test.mode)
				f := first.As("f")
				next := tplan.Export("next", value.From(f))
				second := tplan.CTE("groups", tplan.Select(next.Project()).From(f).Where(tplan.Le(value.From(f), bound.Expr())), test.mode)
				l, r := second.As("l"), second.As("r")
				plan := tplan.DefineRead(out, test.name, tplan.Select(tplan.Project(tplan.Output(out, "first", tplan.Int8()), next.From(r)), tplan.Project(tplan.Output(out, "second", tplan.Int8()), next.From(l))).From(l).InnerJoin(r, tplan.Eq(next.From(l), next.From(r))).With(first, second).OrderBy(tplan.AscExpr(next.From(l))))
				run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
				require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
					rows, err := plan.Bind(s).Find(ctx, tplan.BindValue(bound, int64(2)))
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
			layout := tplan.MapTable(table)
			a, b := layout.As("i"), layout.As("i")
			id := tplan.MappedColumn[int64](layout, "id")
			first, one := tplan.Input("first", tplan.Int8()), tplan.Input("one", tplan.Int8())
			value := tplan.Export("value", id.On(a.Rel()).Expr())
			common := tplan.RecursiveCTE("notes", tplan.Select(value.Project()).From(a).Where(tplan.Eq(id.On(a.Rel()).Expr(), first.Expr())), tplan.RecursiveUnionDistinct, func(self tplan.QuerySource) tplan.ReadSelect {
				return tplan.Select(tplan.Export("value", id.On(b.Rel()).Expr()).Project()).From(self).InnerJoin(b, tplan.Eq(id.On(b.Rel()).Expr(), tplan.Plus(value.From(self), one.Expr())))
			})
			n := common.As("n")
			out := q.DefineView(reg, q.ViewSpec[composedValue]{Name: "entries"})
			plan := tplan.DefineRead(out, "scoped", tplan.Select(tplan.Project(tplan.Output(out, "value", tplan.Int8()), value.From(n))).From(n).With(common))
			for _, test := range []struct {
				scope itemScope
				first int64
				want  []composedValue
			}{{1, 1, []composedValue{{new(int64(1))}}}, {2, 2, []composedValue{{new(int64(2))}}}, {2, 1, nil}} {
				require.NoError(t, runRowPlan(t, db, pool, driver, reg.SQLAuthority(), "", test.scope, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
					rows, err := plan.Bind(s).Find(ctx, tplan.BindValue(first, test.first), tplan.BindValue(one, int64(1)))
					require.NoError(t, err)
					require.Equal(t, test.want, rows)
					return nil
				}))
			}
		})
	}
}
