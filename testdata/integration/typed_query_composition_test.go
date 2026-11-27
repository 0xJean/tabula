//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
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
			layout := q.MapTable(table)
			outer, inner := layout.As("i"), layout.As("i")
			id := q.MappedColumn[int64](layout, "id")
			value := q.MappedColumn[int64](layout, "value")
			out := q.DefineView(reg, q.ViewSpec[composedValue]{Name: "entries"})
			target := q.Output(out, "value", q.Int8())
			one, two := q.Input("value", q.Int8()), q.Input("value", q.Int8())
			sourceFree := q.DefineRead(out, "inputs", q.Select(q.Project(target, q.Plus(one.Expr(), q.ScalarSubquery(q.SelectValue(two.Expr()).Take(1))))))
			bounded := q.DefineRead(out, "bounded", q.Select(q.Project(target, q.ScalarSubquery(q.SelectValue(id.On(inner.Rel()).Expr()).From(inner).OrderBy(q.Desc(id.On(inner.Rel()))).Take(1)))))
			empty := q.DefineRead(out, "empty", q.Select(q.Project(target, q.ScalarSubquery(q.SelectValue(id.On(inner.Rel()).Expr()).From(inner).Where(q.Lt(id.On(inner.Rel()).Expr(), one.Expr()))))))
			msome := q.DefineRead(out, "many", q.Select(q.Project(target, q.ScalarSubquery(q.SelectValue(id.On(inner.Rel()).Expr()).From(inner)))))
			correlated := q.DefineRead(out, "correlated", q.Select(q.Project(target, q.ScalarSubquery(q.SelectValue(value.On(inner.Rel()).Expr()).From(inner).Where(q.Eq(id.On(inner.Rel()).Expr(), q.Outer(id.On(outer.Rel()).Expr())))))).From(outer).OrderBy(q.Asc(id.On(outer.Rel()))))
			exported := q.Export("value", q.Outer(id.On(outer.Rel()).Expr()))
			lateral := q.Derived("n", q.Select(exported.Project()).Where(q.Lt(q.Outer(id.On(outer.Rel()).Expr()), one.Expr())))
			joined := q.DefineRead(out, "lateral", q.Select(q.Project(target, exported.From(lateral))).From(outer).LeftLateral(lateral, q.IsNotDistinct(one.Expr(), one.Expr())).OrderBy(q.Asc(id.On(outer.Rel()))))
			boolOut := q.DefineView(reg, q.ViewSpec[composedTruth]{Name: "notes"})
			bt := q.Output(boolOut, "value", q.Bool())
			exists := q.DefineRead(boolOut, "exists", q.Select(q.Project(bt, q.ExistsQuery(q.Select().From(inner).Where(q.Lt(id.On(inner.Rel()).Expr(), one.Expr()))))))
			rhs := q.SelectValue(value.On(inner.Rel()).Expr()).From(inner).Where(q.Lt(id.On(inner.Rel()).Expr(), two.Expr()))
			some := q.DefineRead(boolOut, "any", q.Select(q.Project(bt, q.AnyQuery(q.CompareEqual, one.Expr(), rhs))))
			all := q.DefineRead(boolOut, "all", q.Select(q.Project(bt, q.AllQuery(q.CompareEqual, one.Expr(), rhs))))
			notIn := q.DefineRead(boolOut, "not_in", q.Select(q.Project(bt, q.Not(q.InQuery(one.Expr(), rhs)))))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				rows, err := sourceFree.Bind(s).Find(ctx, q.BindValue(one, int64(7)), q.BindValue(two, int64(8)))
				require.NoError(t, err)
				require.Equal(t, []composedValue{{new(int64(15))}}, rows)
				rows, err = bounded.Bind(s).Find(ctx)
				require.NoError(t, err)
				require.Equal(t, []composedValue{{new(int64(3))}}, rows)
				rows, err = empty.Bind(s).Find(ctx, q.BindValue(one, int64(0)))
				require.NoError(t, err)
				require.Equal(t, []composedValue{{nil}}, rows)
				rows, err = correlated.Bind(s).Find(ctx)
				require.NoError(t, err)
				require.Equal(t, []composedValue{{new(int64(10))}, {new(int64(20))}, {nil}}, rows)
				rows, err = joined.Bind(s).Find(ctx, q.BindValue(one, int64(2)))
				require.NoError(t, err)
				require.Equal(t, []composedValue{{new(int64(1))}, {nil}, {nil}}, rows)
				for _, test := range []struct {
					bound int64
					want  bool
				}{{0, false}, {2, true}} {
					result, err := exists.Bind(s).Find(ctx, q.BindValue(one, test.bound))
					require.NoError(t, err)
					require.Equal(t, new(test.want), result[0].Value)
				}
				for _, test := range []struct {
					lhs, bound         int64
					some, every, notIn *bool
				}{{10, 0, new(false), new(true), new(true)}, {10, 3, new(true), new(false), new(false)}, {99, 4, nil, new(false), nil}, {10, 4, new(true), new(false), new(false)}} {
					args := []q.Binding{q.BindValue(one, test.lhs), q.BindValue(two, test.bound)}
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
			target := q.Output(out, "value", q.Int8())
			left, right := q.Input("left", q.ArrayOf(q.Int8())), q.Input("right", q.ArrayOf(q.Int8()))
			l, r := q.UnnestColumn("value", left.Expr(), q.Int8()), q.UnnestColumn("value", right.Expr(), q.Int8())
			ls, rs := q.Unnest("l", l), q.Unnest("r", r)
			lf, rf := q.Export("value", l.From(ls)), q.Export("value", r.From(rs))
			a, b := q.Select(lf.Project()).From(ls), q.Select(rf.Project()).From(rs)
			for _, test := range []struct {
				name  string
				build func(q.ReadSelect, q.ReadSelect, ...q.ReadSelect) q.ReadSelect
				want  []composedValue
			}{
				{"union", q.Union, []composedValue{{new(int64(1))}, {new(int64(2))}, {new(int64(3))}}},
				{"union_all", q.UnionAll, []composedValue{{new(int64(1))}, {new(int64(1))}, {new(int64(1))}, {new(int64(2))}, {new(int64(3))}}},
				{"intersect", q.Intersect, []composedValue{{new(int64(1))}}}, {"intersect_all", q.IntersectAll, []composedValue{{new(int64(1))}}},
				{"except", q.Except, []composedValue{{new(int64(2))}}}, {"except_all", q.ExceptAll, []composedValue{{new(int64(1))}, {new(int64(2))}}},
			} {
				source := q.Derived("d", test.build(a, b))
				plan := q.DefineRead(out, test.name, q.Select(q.Project(target, lf.From(source))).From(source).OrderBy(q.AscExpr(lf.From(source))))
				run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
				require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
					rows, err := plan.Bind(s).Find(ctx, q.BindValue(left, []int64{1, 1, 2}), q.BindValue(right, []int64{1, 3}))
					require.NoError(t, err)
					require.Equal(t, test.want, rows)
					return nil
				}))
			}
			first, increment, ceiling := q.Input("first", q.Int8()), q.Input("increment", q.Int8()), q.Input("ceiling", q.Int8())
			value := q.Export("value", first.Expr())
			common := q.RecursiveCTE("notes", q.Select(value.Project()), q.RecursiveUnionAll, func(self q.QuerySource) q.ReadSelect {
				return q.Select(q.Export("value", q.Plus(value.From(self), increment.Expr())).Project()).From(self).Where(q.Lt(value.From(self), ceiling.Expr()))
			})
			n := common.As("n")
			recursive := q.DefineRead(out, "recursive", q.Select(q.Project(target, value.From(n))).From(n).With(common).OrderBy(q.AscExpr(value.From(n))))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				rows, err := recursive.Bind(s).Find(ctx, q.BindValue(first, int64(1)), q.BindValue(increment, int64(1)), q.BindValue(ceiling, int64(4)))
				require.NoError(t, err)
				require.Equal(t, []composedValue{{new(int64(1))}, {new(int64(2))}, {new(int64(3))}, {new(int64(4))}}, rows)
				return nil
			}))
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			err := run(ctx, func(ctx context.Context, s session) error {
				_, err := recursive.Bind(s).Find(ctx, q.BindValue(first, int64(1)), q.BindValue(increment, int64(0)), q.BindValue(ceiling, int64(4)))
				return err
			})
			require.Error(t, err)
		})
	}
}
