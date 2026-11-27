//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

type composedItem struct {
	ID    int64  `db:"id,key"`
	Value *int64 `db:"value"`
}
type composedValue struct {
	Value *int64 `db:"value"`
}
type composedTruth struct {
	Value *bool `db:"value"`
}

func TestQueryCompositionDrivers(t *testing.T) {
	db, pool := databases(t)
	schema := typesSchema(t, db)
	_, err := db.ExecContext(t.Context(), "CREATE TABLE "+schema+".items (id bigint PRIMARY KEY,value bigint); INSERT INTO "+schema+".items VALUES (1,10),(2,20),(3,NULL)")
	require.NoError(t, err)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			cfg := q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL, ArrayColumn: pgxdriver.ArrayColumn}
			if driver == "pq" {
				cfg.ArrayArg = pqdriver.Array
				cfg.ArrayColumn = pqdriver.ArrayColumn
			}
			reg := q.NewRegistry(cfg)
			table := q.DefinePlanSource(reg, q.TableSpec[composedItem, int64]{Name: schema + ".items", RelationScope: q.Global()})
			layout := tplan.MapTable(table)
			outer, inner := layout.As("i"), layout.As("i")
			id := tplan.MappedColumn[int64](layout, "id")
			value := tplan.MappedColumn[int64](layout, "value")
			out := q.DefineView(reg, q.ViewSpec[composedValue]{Name: "entries"})
			target := tplan.Output(out, "value", tplan.Int8())
			one, two := tplan.Input("value", tplan.Int8()), tplan.Input("value", tplan.Int8())
			sourceFree := tplan.DefineRead(out, "inputs", tplan.Select(tplan.Project(target, tplan.Plus(one.Expr(), tplan.ScalarSubquery(tplan.SelectValue(two.Expr()).Take(1))))))
			bounded := tplan.DefineRead(out, "bounded", tplan.Select(tplan.Project(target, tplan.ScalarSubquery(tplan.SelectValue(id.On(inner.Rel()).Expr()).From(inner).OrderBy(tplan.Desc(id.On(inner.Rel()))).Take(1)))))
			empty := tplan.DefineRead(out, "empty", tplan.Select(tplan.Project(target, tplan.ScalarSubquery(tplan.SelectValue(id.On(inner.Rel()).Expr()).From(inner).Where(tplan.Lt(id.On(inner.Rel()).Expr(), one.Expr()))))))
			msome := tplan.DefineRead(out, "many", tplan.Select(tplan.Project(target, tplan.ScalarSubquery(tplan.SelectValue(id.On(inner.Rel()).Expr()).From(inner)))))
			correlated := tplan.DefineRead(out, "correlated", tplan.Select(tplan.Project(target, tplan.ScalarSubquery(tplan.SelectValue(value.On(inner.Rel()).Expr()).From(inner).Where(tplan.Eq(id.On(inner.Rel()).Expr(), tplan.Outer(id.On(outer.Rel()).Expr())))))).From(outer).OrderBy(tplan.Asc(id.On(outer.Rel()))))
			exported := tplan.Export("value", tplan.Outer(id.On(outer.Rel()).Expr()))
			lateral := tplan.Derived("n", tplan.Select(exported.Project()).Where(tplan.Lt(tplan.Outer(id.On(outer.Rel()).Expr()), one.Expr())))
			joined := tplan.DefineRead(out, "lateral", tplan.Select(tplan.Project(target, exported.From(lateral))).From(outer).LeftLateral(lateral, tplan.IsNotDistinct(one.Expr(), one.Expr())).OrderBy(tplan.Asc(id.On(outer.Rel()))))
			boolOut := q.DefineView(reg, q.ViewSpec[composedTruth]{Name: "notes"})
			bt := tplan.Output(boolOut, "value", tplan.Bool())
			exists := tplan.DefineRead(boolOut, "exists", tplan.Select(tplan.Project(bt, tplan.ExistsQuery(tplan.Select().From(inner).Where(tplan.Lt(id.On(inner.Rel()).Expr(), one.Expr()))))))
			rhs := tplan.SelectValue(value.On(inner.Rel()).Expr()).From(inner).Where(tplan.Lt(id.On(inner.Rel()).Expr(), two.Expr()))
			some := tplan.DefineRead(boolOut, "any", tplan.Select(tplan.Project(bt, tplan.AnyQuery(tplan.CompareEqual, one.Expr(), rhs))))
			all := tplan.DefineRead(boolOut, "all", tplan.Select(tplan.Project(bt, tplan.AllQuery(tplan.CompareEqual, one.Expr(), rhs))))
			notIn := tplan.DefineRead(boolOut, "not_in", tplan.Select(tplan.Project(bt, tplan.Not(tplan.InQuery(one.Expr(), rhs)))))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				rows, err := sourceFree.Bind(s).Find(ctx, tplan.BindValue(one, int64(7)), tplan.BindValue(two, int64(8)))
				require.NoError(t, err)
				require.Equal(t, []composedValue{{new(int64(15))}}, rows)
				rows, err = bounded.Bind(s).Find(ctx)
				require.NoError(t, err)
				require.Equal(t, []composedValue{{new(int64(3))}}, rows)
				rows, err = empty.Bind(s).Find(ctx, tplan.BindValue(one, int64(0)))
				require.NoError(t, err)
				require.Equal(t, []composedValue{{nil}}, rows)
				rows, err = correlated.Bind(s).Find(ctx)
				require.NoError(t, err)
				require.Equal(t, []composedValue{{new(int64(10))}, {new(int64(20))}, {nil}}, rows)
				rows, err = joined.Bind(s).Find(ctx, tplan.BindValue(one, int64(2)))
				require.NoError(t, err)
				require.Equal(t, []composedValue{{new(int64(1))}, {nil}, {nil}}, rows)
				for _, test := range []struct {
					bound int64
					want  bool
				}{{0, false}, {2, true}} {
					result, err := exists.Bind(s).Find(ctx, tplan.BindValue(one, test.bound))
					require.NoError(t, err)
					require.Equal(t, new(test.want), result[0].Value)
				}
				for _, test := range []struct {
					lhs, bound         int64
					some, every, notIn *bool
				}{{10, 0, new(false), new(true), new(true)}, {10, 3, new(true), new(false), new(false)}, {99, 4, nil, new(false), nil}, {10, 4, new(true), new(false), new(false)}} {
					args := []tplan.Binding{tplan.BindValue(one, test.lhs), tplan.BindValue(two, test.bound)}
					a, err := some.Bind(s).Find(ctx, args...)
					require.NoError(t, err)
					require.Equal(t, test.some, a[0].Value)
					b, err := all.Bind(s).Find(ctx, args...)
					require.NoError(t, err)
					require.Equal(t, test.every, b[0].Value)
					c, err := notIn.Bind(s).Find(ctx, args...)
					require.NoError(t, err)
					require.Equal(t, test.notIn, c[0].Value)
				}
				return nil
			}))
			err = run(t.Context(), func(ctx context.Context, s session) error { _, err := msome.Bind(s).Find(ctx); return err })
			require.Equal(t, "21000", q.SQLState(err))
			work := sqlcheck.RoleStatements{}
			work.AddDefinitions("", sqlcheck.ViewsToDefinitions(reg.DefinedViews())...)
			// PREPARE below checks exact globally dense parameters independently of
			// decoding; the stored statement is the same one used by both drivers.
			tx, err := db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, tx.Rollback()) }()
			sql := out.Definition().Statements["inputs.global.data"]
			_, err = tx.ExecContext(t.Context(), "PREPARE query_inputs AS "+sql)
			require.NoError(t, err)
			var types string
			err = tx.QueryRowContext(t.Context(), "SELECT parameter_types::oid[]::text FROM pg_prepared_statements WHERE name='query_inputs'").Scan(&types)
			require.NoError(t, err)
			require.Equal(t, "{20,20,20}", types)
			_, err = tx.ExecContext(t.Context(), "DEALLOCATE query_inputs")
			require.NoError(t, err)
		})
	}
}

func TestQuerySetsAndRecursionDrivers(t *testing.T) {
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
			left, right := tplan.Input("left", q.ArrayOf(tplan.Int8())), tplan.Input("right", q.ArrayOf(tplan.Int8()))
			l, r := tplan.UnnestColumn("value", left.Expr(), tplan.Int8()), tplan.UnnestColumn("value", right.Expr(), tplan.Int8())
			ls, rs := tplan.Unnest("l", l), tplan.Unnest("r", r)
			lf, rf := tplan.Export("value", l.From(ls)), tplan.Export("value", r.From(rs))
			a, b := tplan.Select(lf.Project()).From(ls), tplan.Select(rf.Project()).From(rs)
			for _, test := range []struct {
				name  string
				build func(tplan.ReadSelect, tplan.ReadSelect, ...tplan.ReadSelect) tplan.ReadSelect
				want  []composedValue
			}{
				{"union", tplan.Union, []composedValue{{new(int64(1))}, {new(int64(2))}, {new(int64(3))}}},
				{"union_all", tplan.UnionAll, []composedValue{{new(int64(1))}, {new(int64(1))}, {new(int64(1))}, {new(int64(2))}, {new(int64(3))}}},
				{"intersect", tplan.Intersect, []composedValue{{new(int64(1))}}}, {"intersect_all", tplan.IntersectAll, []composedValue{{new(int64(1))}}},
				{"except", tplan.Except, []composedValue{{new(int64(2))}}}, {"except_all", tplan.ExceptAll, []composedValue{{new(int64(1))}, {new(int64(2))}}},
			} {
				source := tplan.Derived("d", test.build(a, b))
				plan := tplan.DefineRead(out, test.name, tplan.Select(tplan.Project(target, lf.From(source))).From(source).OrderBy(tplan.AscExpr(lf.From(source))))
				run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
				require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
					rows, err := plan.Bind(s).Find(ctx, tplan.BindValue(left, []int64{1, 1, 2}), tplan.BindValue(right, []int64{1, 3}))
					require.NoError(t, err)
					require.Equal(t, test.want, rows)
					return nil
				}))
			}
			first, increment, ceiling := tplan.Input("first", tplan.Int8()), tplan.Input("increment", tplan.Int8()), tplan.Input("ceiling", tplan.Int8())
			value := tplan.Export("value", first.Expr())
			common := tplan.RecursiveCTE("notes", tplan.Select(value.Project()), tplan.RecursiveUnionAll, func(self tplan.QuerySource) tplan.ReadSelect {
				return tplan.Select(tplan.Export("value", tplan.Plus(value.From(self), increment.Expr())).Project()).From(self).Where(tplan.Lt(value.From(self), ceiling.Expr()))
			})
			n := common.As("n")
			recursive := tplan.DefineRead(out, "recursive", tplan.Select(tplan.Project(target, value.From(n))).From(n).With(common).OrderBy(tplan.AscExpr(value.From(n))))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				rows, err := recursive.Bind(s).Find(ctx, tplan.BindValue(first, int64(1)), tplan.BindValue(increment, int64(1)), tplan.BindValue(ceiling, int64(4)))
				require.NoError(t, err)
				require.Equal(t, []composedValue{{new(int64(1))}, {new(int64(2))}, {new(int64(3))}, {new(int64(4))}}, rows)
				return nil
			}))
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			err := run(ctx, func(ctx context.Context, s session) error {
				_, err := recursive.Bind(s).Find(ctx, tplan.BindValue(first, int64(1)), tplan.BindValue(increment, int64(0)), tplan.BindValue(ceiling, int64(4)))
				return err
			})
			require.Error(t, err)
		})
	}
}
