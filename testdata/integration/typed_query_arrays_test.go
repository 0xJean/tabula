//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/pgxdriver"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/stretchr/testify/require"
)

func TestQueryArraySourcesDrivers(t *testing.T) {
	type expanded struct {
		Number   *int64  `db:"number"`
		Label    *string `db:"label"`
		Position int64   `db:"position"`
	}
	type arrays struct {
		Values []*int64 `db:"values,array"`
	}
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			cfg := q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL, ArrayColumn: pgxdriver.ArrayColumn}
			if driver == "pq" {
				cfg.ArrayArg = pqdriver.Array
				cfg.ArrayColumn = pqdriver.ArrayColumn
			}
			reg := q.NewRegistry(cfg)
			calls := 0
			numbersType := q.ArrayOfNullable(tplan.Int8()).WithEncoder(func(v []*int64) (any, error) { calls++; return v, nil })
			numbers, labels := tplan.Input("array", numbersType), tplan.Input("array", q.ArrayOf(tplan.Text()))
			n, l, o := tplan.UnnestColumn("number", numbers.Expr(), tplan.Int8()), tplan.UnnestColumn("label", labels.Expr(), tplan.Text()), tplan.UnnestOrdinality("position")
			source := tplan.Unnest("u", o, n, l)
			out := q.DefineView(reg, q.ViewSpec[expanded]{Name: "entries"})
			plan := tplan.DefineRead(out, "expand", tplan.Select(tplan.Project(tplan.Output(out, "number", tplan.Int8()), n.From(source)), tplan.Project(tplan.Output(out, "label", tplan.Text()), l.From(source)), tplan.Project(tplan.Output(out, "position", tplan.Int8()), o.From(source))).From(source).OrderBy(tplan.AscExpr(o.From(source))))
			arrayOut := q.DefineView(reg, q.ViewSpec[arrays]{Name: "notes"})
			array := tplan.DefineRead(arrayOut, "array", tplan.Select(tplan.Project(tplan.Output(arrayOut, "values", q.ArrayOfNullable(tplan.Int8())), tplan.ArraySubqueryAs(q.ArrayOfNullable(tplan.Int8()), tplan.SelectValue(n.From(source)).From(source).OrderBy(tplan.DescExpr(o.From(source)))))))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			original := []*int64{new(int64(7)), nil, new(int64(9))}
			binding := tplan.BindValue(numbers, original)
			*original[0] = 100
			original[2] = new(int64(200))
			require.Equal(t, 1, calls)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				for range 2 {
					rows, err := plan.Bind(s).Find(ctx, binding, tplan.BindValue(labels, []string{"a"}))
					require.NoError(t, err)
					require.Equal(t, []expanded{{new(int64(7)), new("a"), 1}, {nil, nil, 2}, {new(int64(9)), nil, 3}}, rows)
					values, err := array.Bind(s).Find(ctx, binding, tplan.BindValue(labels, []string{"a"}))
					require.NoError(t, err)
					require.Equal(t, []arrays{{[]*int64{new(int64(9)), nil, new(int64(7))}}}, values)
				}
				require.Equal(t, 1, calls)
				for _, empty := range [][]*int64{nil, {}} {
					rows, err := plan.Bind(s).Find(ctx, tplan.BindValue(numbers, empty), tplan.BindValue(labels, []string{"a", "b"}))
					require.NoError(t, err)
					require.Equal(t, []expanded{{nil, new("a"), 1}, {nil, new("b"), 2}}, rows)
					values, err := array.Bind(s).Find(ctx, tplan.BindValue(numbers, empty), tplan.BindValue(labels, []string{}))
					require.NoError(t, err)
					require.Len(t, values, 1)
					require.NotNil(t, values[0].Values)
					require.Empty(t, values[0].Values)
				}
				return nil
			}))
		})
	}
}

func TestQueryMaterializedVolatileArrayDrivers(t *testing.T) {
	type item struct {
		ID int64 `db:"id,key"`
	}
	type values struct {
		First  []string `db:"first,array"`
		Second []string `db:"second,array"`
		Value  []string `db:"value,array"`
	}
	db, pool := databases(t)
	schema := typesSchema(t, db)
	_, err := db.ExecContext(t.Context(), "CREATE TABLE "+schema+".items(id bigint PRIMARY KEY); INSERT INTO "+schema+".items VALUES (1),(2),(3)")
	require.NoError(t, err)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			cfg := q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL, ArrayColumn: pgxdriver.ArrayColumn}
			if driver == "pq" {
				cfg.ArrayArg = pqdriver.Array
				cfg.ArrayColumn = pqdriver.ArrayColumn
			}
			reg := q.NewRegistry(cfg)
			table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: schema + ".items", RelationScope: q.Global()})
			source := tplan.MapTable(table).As("i")
			generated := tplan.Export("value", tplan.ArrayExpr(tplan.GenRandomUUID()))
			common := tplan.CTE("notes", tplan.Select(generated.Project()).From(source), tplan.Materialized)
			n := common.As("n")
			kind := pg.ArrayOf(pg.UUID[string]())
			other := tplan.Input("other", kind)
			out := q.DefineView(reg, q.ViewSpec[values]{Name: "entries"})
			query := tplan.Select(tplan.Project(tplan.Output(out, "first", kind), generated.From(n)), tplan.Project(tplan.Output(out, "second", kind), generated.From(n)), tplan.Project(tplan.Output(out, "value", kind), tplan.NullIf(generated.From(n), other.Expr()))).From(n).With(common)
			plan := tplan.DefineRead(out, "materialized", query)
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				rows, err := plan.Bind(s).Find(ctx, tplan.BindValue(other, []string{}))
				require.NoError(t, err)
				require.Len(t, rows, 3)
				seen := make(map[string]bool)
				for _, row := range rows {
					require.Len(t, row.First, 1)
					require.Equal(t, row.First, row.Second)
					require.Equal(t, row.First, row.Value)
					require.False(t, seen[row.First[0]])
					seen[row.First[0]] = true
				}
				return nil
			}))

			threshold := tplan.Input("threshold", tplan.Int8())
			id := tplan.FieldOf(source.Rel(), "id", tplan.Int8())
			lateralQuery := func(correlated bool) (tplan.ReadSelect, tplan.Exported[[]string]) {
				seed := tplan.Select(generated.Project())
				if correlated {
					seed = seed.Where(tplan.Gt(tplan.Outer(tplan.Outer(id.Expr())), threshold.Expr()))
				}
				common := tplan.CTE("notes", seed, tplan.Materialized)
				n := common.As("n")
				stored := tplan.Export("stored", generated.From(n))
				lateral := tplan.Derived("d", tplan.Select(stored.Project()).From(n).With(common))
				result := tplan.Select(tplan.Project(tplan.Output(out, "first", kind), stored.From(lateral)), tplan.Project(tplan.Output(out, "second", kind), stored.From(lateral)), tplan.Project(tplan.Output(out, "value", kind), tplan.NullIf(stored.From(lateral), other.Expr()))).From(source).CrossLateral(lateral)
				return result, stored
			}
			correlatedQuery, _ := lateralQuery(true)
			perRow := tplan.DefineRead(out, "per_row", correlatedQuery)
			uncorrelatedQuery, _ := lateralQuery(false)
			once := tplan.DefineRead(out, "uncorrelated", uncorrelatedQuery)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				rows, err := perRow.Bind(s).Find(ctx, tplan.BindValue(threshold, int64(0)), tplan.BindValue(other, []string{}))
				require.NoError(t, err)
				require.Len(t, rows, 3)
				seen := make(map[string]bool)
				for _, row := range rows {
					require.Equal(t, row.First, row.Second)
					require.Equal(t, row.First, row.Value)
					require.False(t, seen[row.First[0]])
					seen[row.First[0]] = true
				}
				rows, err = once.Bind(s).Find(ctx, tplan.BindValue(other, []string{}))
				require.NoError(t, err)
				require.Len(t, rows, 3)
				require.Equal(t, rows[0], rows[1])
				require.Equal(t, rows[1], rows[2])
				return nil
			}))
			before := out.Definition()
			_, err = tplan.TryDefineRead(out, "direct", tplan.Select(tplan.Project(tplan.Output(out, "first", kind), tplan.ArrayExpr(tplan.GenRandomUUID())), tplan.Project(tplan.Output(out, "second", kind), tplan.ArrayExpr(tplan.GenRandomUUID())), tplan.Project(tplan.Output(out, "value", kind), tplan.NullIf(tplan.ArrayExpr(tplan.GenRandomUUID()), other.Expr()))))
			require.Error(t, err)
			require.Equal(t, before, out.Definition())
		})
	}
}
