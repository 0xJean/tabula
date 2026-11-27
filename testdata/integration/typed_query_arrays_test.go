//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/pgxdriver"
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
			numbersType := q.ArrayOfNullable(q.Int8()).WithEncoder(func(v []*int64) (any, error) { calls++; return v, nil })
			numbers, labels := q.Input("array", numbersType), q.Input("array", q.ArrayOf(q.Text()))
			n, l, o := q.UnnestColumn("number", numbers.Expr(), q.Int8()), q.UnnestColumn("label", labels.Expr(), q.Text()), q.UnnestOrdinality("position")
			source := q.Unnest("u", o, n, l)
			out := q.DefineView(reg, q.ViewSpec[expanded]{Name: "entries"})
			plan := q.DefineRead(out, "expand", q.Select(q.Project(q.Output(out, "number", q.Int8()), n.From(source)), q.Project(q.Output(out, "label", q.Text()), l.From(source)), q.Project(q.Output(out, "position", q.Int8()), o.From(source))).From(source).OrderBy(q.AscExpr(o.From(source))))
			arrayOut := q.DefineView(reg, q.ViewSpec[arrays]{Name: "notes"})
			array := q.DefineRead(arrayOut, "array", q.Select(q.Project(q.Output(arrayOut, "values", q.ArrayOfNullable(q.Int8())), q.ArraySubqueryAs(q.ArrayOfNullable(q.Int8()), q.SelectValue(n.From(source)).From(source).OrderBy(q.DescExpr(o.From(source)))))))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			original := []*int64{new(int64(7)), nil, new(int64(9))}
			binding := q.BindValue(numbers, original)
			*original[0] = 100
			original[2] = new(int64(200))
			require.Equal(t, 1, calls)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				for range 2 {
					rows, err := plan.Bind(s).Find(ctx, binding, q.BindValue(labels, []string{"a"}))
					require.NoError(t, err)
					require.Equal(t, []expanded{{new(int64(7)), new("a"), 1}, {nil, nil, 2}, {new(int64(9)), nil, 3}}, rows)
					values, err := array.Bind(s).Find(ctx, binding, q.BindValue(labels, []string{"a"}))
					require.NoError(t, err)
					require.Equal(t, []arrays{{[]*int64{new(int64(9)), nil, new(int64(7))}}}, values)
				}
				require.Equal(t, 1, calls)
				for _, empty := range [][]*int64{nil, {}} {
					rows, err := plan.Bind(s).Find(ctx, q.BindValue(numbers, empty), q.BindValue(labels, []string{"a", "b"}))
					require.NoError(t, err)
					require.Equal(t, []expanded{{nil, new("a"), 1}, {nil, new("b"), 2}}, rows)
					values, err := array.Bind(s).Find(ctx, q.BindValue(numbers, empty), q.BindValue(labels, []string{}))
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
			source := q.MapTable(table).As("i")
			generated := q.Export("value", q.ArrayExpr(q.GenRandomUUID()))
			common := q.CTE("notes", q.Select(generated.Project()).From(source), q.Materialized)
			n := common.As("n")
			kind := pg.ArrayOf(pg.UUID[string]())
			other := q.Input("other", kind)
			out := q.DefineView(reg, q.ViewSpec[values]{Name: "entries"})
			query := q.Select(q.Project(q.Output(out, "first", kind), generated.From(n)), q.Project(q.Output(out, "second", kind), generated.From(n)), q.Project(q.Output(out, "value", kind), q.NullIf(generated.From(n), other.Expr()))).From(n).With(common)
			plan := q.DefineRead(out, "materialized", query)
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				rows, err := plan.Bind(s).Find(ctx, q.BindValue(other, []string{}))
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

			threshold := q.Input("threshold", q.Int8())
			id := q.FieldOf(source.Rel(), "id", q.Int8())
			lateralQuery := func(correlated bool) (q.ReadSelect, q.Exported[[]string]) {
				seed := q.Select(generated.Project())
				if correlated {
					seed = seed.Where(q.Gt(q.Outer(q.Outer(id.Expr())), threshold.Expr()))
				}
				common := q.CTE("notes", seed, q.Materialized)
				n := common.As("n")
				stored := q.Export("stored", generated.From(n))
				lateral := q.Derived("d", q.Select(stored.Project()).From(n).With(common))
				result := q.Select(q.Project(q.Output(out, "first", kind), stored.From(lateral)), q.Project(q.Output(out, "second", kind), stored.From(lateral)), q.Project(q.Output(out, "value", kind), q.NullIf(stored.From(lateral), other.Expr()))).From(source).CrossLateral(lateral)
				return result, stored
			}
			correlatedQuery, _ := lateralQuery(true)
			perRow := q.DefineRead(out, "per_row", correlatedQuery)
			uncorrelatedQuery, _ := lateralQuery(false)
			once := q.DefineRead(out, "uncorrelated", uncorrelatedQuery)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				rows, err := perRow.Bind(s).Find(ctx, q.BindValue(threshold, int64(0)), q.BindValue(other, []string{}))
				require.NoError(t, err)
				require.Len(t, rows, 3)
				seen := make(map[string]bool)
				for _, row := range rows {
					require.Equal(t, row.First, row.Second)
					require.Equal(t, row.First, row.Value)
					require.False(t, seen[row.First[0]])
					seen[row.First[0]] = true
				}
				rows, err = once.Bind(s).Find(ctx, q.BindValue(other, []string{}))
				require.NoError(t, err)
				require.Len(t, rows, 3)
				require.Equal(t, rows[0], rows[1])
				require.Equal(t, rows[1], rows[2])
				return nil
			}))
			before := out.Definition()
			_, err = q.TryDefineRead(out, "direct", q.Select(q.Project(q.Output(out, "first", kind), q.ArrayExpr(q.GenRandomUUID())), q.Project(q.Output(out, "second", kind), q.ArrayExpr(q.GenRandomUUID())), q.Project(q.Output(out, "value", kind), q.NullIf(q.ArrayExpr(q.GenRandomUUID()), other.Expr()))))
			require.Error(t, err)
			require.Equal(t, before, out.Definition())
		})
	}
}
