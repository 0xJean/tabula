//go:build integration

package tabula

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// The optimizer may legitimately change its costs or node names between
// releases. Compare both prepared queries on the same server and statistics.
type accessPlan struct {
	Node      string `json:"Node Type"`
	Index     string `json:"Index Name"`
	Condition string `json:"Index Cond"`
	Plans     []accessPlan
}

func indexPath(plan accessPlan) []string {
	step := plan.Node
	if plan.Index != "" {
		step += " " + plan.Index
		if plan.Condition != "" {
			step += " condition"
		}
	}
	path := []string{step}
	for _, child := range plan.Plans {
		path = append(path, indexPath(child)...)
	}
	return path
}

func TestTypedParameterIndexesPostgres(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	exec := func(t *testing.T, query string) {
		t.Helper()
		_, err := tx.ExecContext(ctx, query)
		require.NoError(t, err)
	}
	exec(t, "CREATE DOMAIN pg_temp.rank AS bigint")
	// A selective distribution lets PostgreSQL choose among ordinary plans.
	var states []string
	for i := range 1024 {
		states = append(states, fmt.Sprintf("'s%04d'", i))
	}
	exec(t, "CREATE TYPE pg_temp.item_state AS ENUM ("+strings.Join(states, ",")+")")
	exec(t, "CREATE TEMP TABLE items(rank pg_temp.rank, state pg_temp.item_state, label varchar(8))")
	exec(t, `INSERT INTO items SELECT n, ('s'||lpad((n%1024)::text,4,'0'))::pg_temp.item_state, lpad(n::text,8,'0') FROM generate_series(1,8192) n`)
	for _, col := range []string{"rank", "state", "label"} {
		exec(t, "CREATE INDEX items_"+col+" ON items ("+col+")")
	}
	exec(t, "ANALYZE items")
	exec(t, "SET LOCAL plan_cache_mode = force_generic_plan")
	// This single connection owns the prepared statements and their plans.
	explain := func(t *testing.T, name string, values []string) []string {
		t.Helper()
		var raw []byte
		require.NoError(t, tx.QueryRowContext(ctx, "EXPLAIN (FORMAT JSON,COSTS OFF) EXECUTE "+name+"("+strings.Join(values, ",")+")").Scan(&raw))
		var result []struct{ Plan accessPlan }
		require.NoError(t, json.Unmarshal(raw, &result))
		require.Len(t, result, 1)
		return indexPath(result[0].Plan)
	}
	// A projection and an operator between two otherwise unconstrained slots
	// must agree with both PostgreSQL's parameter OIDs and its result value.
	scalars, err := newTypedCompiler(false)
	require.NoError(t, err)
	left, right := Input("left", Int8()), Input("right", Int8())
	projection, err := left.Expr().part.render(scalars)
	require.NoError(t, err)
	less, err := typedComparison("<", left.Expr(), right.Expr(), false).part.render(scalars)
	require.NoError(t, err)
	query := "SELECT " + projection + ", " + less
	exec(t, "PREPARE compare_values AS "+query)
	var oidsMatch, compared bool
	var value int64
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT parameter_types=ARRAY['bigint'::regtype,'bigint'::regtype] FROM pg_prepared_statements WHERE name='compare_values'").Scan(&oidsMatch))
	require.True(t, oidsMatch)
	require.NoError(t, tx.QueryRowContext(ctx, query, 2, 10).Scan(&value, &compared))
	require.EqualValues(t, 2, value)
	require.True(t, compared)
	reg := NewRegistry(RegistryConfig[struct{}]{})
	type indexItem struct {
		Rank  int64  `db:"rank,key"`
		State string `db:"state"`
		Label string `db:"label"`
	}
	table := Define(reg, TableSpec[indexItem, int64]{Name: "pg_temp.items", RelationScope: Global()})
	rel := FromTable(table, "i", Global())
	rank := CatalogType[int64]("pg_temp", "rank", nil)
	state := CatalogType[string]("pg_temp", "item_state", nil)
	label := Varchar(8)
	type columnCase struct {
		field  *fieldRef
		kind   catalogType
		values []string
	}
	for _, data := range []columnCase{
		{FieldOf(rel, "rank", rank).ref, rank.id, []string{"42", "43"}},
		{FieldOf(rel, "state", state).ref, state.id, []string{"'s0042'", "'s0043'"}},
		{FieldOf(rel, "label", label).ref, label.id, []string{"'00000042'", "'00000043'"}},
	} {
		t.Run(data.field.name, func(t *testing.T) {
			for _, op := range []string{"equal", "range", "in", "any"} {
				t.Run(op, func(t *testing.T) {
					c, err := newTypedCompiler(false, rel.source)
					require.NoError(t, err)
					col, err := data.field.render(c)
					require.NoError(t, err)
					expectedArgs := 1
					if op == "in" || op == "range" {
						expectedArgs = 2
					}
					kind := data.kind
					values := data.values[:expectedArgs]
					if op == "any" {
						kind = CatalogType[[]string](kind.schema, "_"+kind.name, nil).id
						pair := []string{strings.Trim(data.values[0], "'"), strings.Trim(data.values[1], "'")}
						values = []string{"'{" + strings.Join(pair, ",") + "}'"}
					}
					var slots, plain []string
					for i := range expectedArgs {
						param := &typedInput{name: fmt.Sprintf("value%d", i), kind: kind}
						sql, err := param.render(c)
						require.NoError(t, err)
						slots = append(slots, sql)
						plain = append(plain, fmt.Sprintf("$%d", i+1))
					}
					stmt := func(params []string) string {
						var predicate string
						switch op {
						case "equal":
							predicate = col + " = " + params[0]
						case "range":
							predicate = col + " >= " + params[0] + " AND " + col + " < " + params[1]
						case "in":
							predicate = col + " IN (" + strings.Join(params, ",") + ")"
						case "any":
							predicate = col + " = ANY(" + params[0] + ")"
						}
						return "SELECT " + col + ` FROM pg_temp.items AS "i" WHERE ` + predicate + " ORDER BY " + col + " LIMIT 1"
					}
					meta, err := c.readRequirements()
					require.NoError(t, err)
					shape, err := c.finishShape("find", stmt(slots), catalogType{}, meta)
					require.NoError(t, err)
					exec(t, "PREPARE plain_items AS "+stmt(plain))
					exec(t, "PREPARE typed_items AS "+shape.sql)
					t.Cleanup(func() {
						_, _ = tx.ExecContext(ctx, "DEALLOCATE plain_items")
						_, _ = tx.ExecContext(ctx, "DEALLOCATE typed_items")
					})
					require.Len(t, shape.shape.Inputs, expectedArgs)
					var count int
					require.NoError(t, tx.QueryRowContext(ctx, "SELECT cardinality(parameter_types) FROM pg_prepared_statements WHERE name='typed_items'").Scan(&count))
					require.Equal(t, expectedArgs, count)
					for i, input := range shape.shape.Inputs {
						var exact bool
						require.NoError(t, tx.QueryRowContext(ctx, `SELECT parameter_types[$1]::oid = pg_catalog.to_regtype($2)::oid
FROM pg_prepared_statements WHERE name='typed_items'`, i+1, input.Type).Scan(&exact))
						require.True(t, exact, "parameter %d has the wrong OID", i+1)
					}
					want, got := explain(t, "plain_items", values), explain(t, "typed_items", values)
					require.Equal(t, want, got)
					idx := "items_" + data.field.name + " condition"
					require.True(t, slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, idx) }), "%s: %v", idx, got)
				})
			}
		})
	}
	c, err := newTypedCompiler(false, rel.source)
	require.NoError(t, err)
	v := Input("label", label)
	pred, err := Eq(FieldOf(rel, "label", label).Expr(), v.Expr()).part.render(c)
	require.NoError(t, err)
	stmt := "SELECT count(*) FROM pg_temp.items AS i WHERE " + pred
	exec(t, "PREPARE label_match AS "+stmt)
	var count int
	require.NoError(t, tx.QueryRowContext(ctx, stmt, "00000042").Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, tx.QueryRowContext(ctx, stmt, "00000042extra").Scan(&count))
	require.Zero(t, count)
	meta, err := c.readRequirements()
	require.NoError(t, err)
	shape, err := c.finishShape("by_label", stmt, catalogType{}, meta)
	require.NoError(t, err)
	col := shape.shape.PhysicalColumns[0]
	require.NotNil(t, col.TypeMod)
	var matches bool
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT atttypid=pg_catalog.to_regtype($1) AND atttypmod=$2 FROM pg_attribute
WHERE attrelid='pg_temp.items'::regclass AND attname='label'`, col.Type, col.TypeMod).Scan(&matches))
	require.True(t, matches)
	original, err := v.Expr().part.render(c)
	require.NoError(t, err)
	exec(t, "SAVEPOINT before_write")
	_, err = tx.ExecContext(ctx, "UPDATE items SET label="+original+" WHERE rank=42", "00000042extra")
	require.Equal(t, "22001", SQLState(err))
	exec(t, "ROLLBACK TO before_write")
	var after string
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT label FROM items WHERE rank=42").Scan(&after))
	require.Equal(t, "00000042", after)
}
