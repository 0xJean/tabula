//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func TestNullableArrayScalarCodecs(t *testing.T) {
	type label string
	type rank int16
	db, pool := databases(t)
	checkNullableArrayScalar(t, db, pool, "bool", false)
	checkNullableArrayScalar(t, db, pool, "text", label("NULL"))
	checkNullableArrayScalar(t, db, pool, "int2", rank(-17))
	checkNullableArrayScalar(t, db, pool, "int4", int32(-31))
	checkNullableArrayScalar(t, db, pool, "int8", int64(1)<<35)
	checkNullableArrayScalar(t, db, pool, "oid", uint32(1)<<31)
	checkNullableArrayScalar(t, db, pool, "float4", float32(1.5))
	checkNullableArrayScalar(t, db, pool, "float8", float64(-2.25))
}

func checkNullableArrayScalar[E any](t *testing.T, db *sql.DB, pool *pgxpool.Pool, catalog string, value E) {
	t.Helper()
	t.Run(catalog, func(t *testing.T) {
		query := "SELECT CAST($1 AS pg_catalog." + catalog + "[])"
		roundTripArray(t, db, pool, query, []E{value})
		roundTripArray(t, db, pool, query, []*E{nil, &value})
	})
}

func roundTripArray[S any](t *testing.T, db *sql.DB, pool *pgxpool.Pool, query string, input S) {
	t.Helper()
	sqlColumn, err := pqdriver.ArrayColumn(reflect.TypeFor[S]())
	require.NoError(t, err)
	var fromSQL S
	err = db.QueryRowContext(t.Context(), query, sqlColumn(&input)).Scan(sqlColumn(&fromSQL))
	require.NoError(t, err)
	require.Equal(t, input, fromSQL)
	pgxColumn, err := pgxdriver.ArrayColumn(reflect.TypeFor[S]())
	require.NoError(t, err)
	var fromPGX S
	err = pool.QueryRow(t.Context(), query, input).Scan(pgxColumn(&fromPGX))
	require.NoError(t, err)
	require.Equal(t, input, fromPGX)
}

func TestNullableArrayElementsOnMappedReads(t *testing.T) {
	type item struct {
		ID    int64    `db:"id,key"`
		Ranks []*int32 `db:"ranks,array"`
	}
	type result struct {
		Ranks []*int32 `db:"ranks,array"`
	}
	db, pool := databases(t)
	tableName := typesSchema(t, db) + ".items"
	_, err := db.ExecContext(t.Context(), "CREATE TABLE "+tableName+" (id bigint PRIMARY KEY, ranks int4[])")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "INSERT INTO "+tableName+" VALUES (1,NULL),(2,'{}'),(3,ARRAY[0,NULL,7])")
	require.NoError(t, err)
	for _, driverName := range []string{"pq", "pgx"} {
		t.Run(driverName, func(t *testing.T) {
			cfg := q.RegistryConfig[struct{}]{ArrayColumn: pgxdriver.ArrayColumn, Deny: q.DenyLegacySQL}
			if driverName == "pq" {
				cfg.ArrayArg, cfg.ArrayColumn = pqdriver.Array, pqdriver.ArrayColumn
			}
			reg := q.NewRegistry(cfg)
			kind := pg.ArrayOfNullable(pg.Int4())
			tab := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: tableName, RelationScope: q.Global()})
			layout := plan.MapTable(tab, plan.ColumnType("ranks", kind))
			src := layout.As("i")
			id := plan.MappedColumn[int64](layout, "id").On(src.Rel())
			ranks := plan.MappedColumn[[]*int32](layout, "ranks").On(src.Rel())
			rows := plan.DefineTableRead(src, "records", plan.SelectColumns(ranks.Column(), id.Column()), plan.Select().OrderBy(plan.Asc(id)))
			in, key := plan.Input("values", kind), plan.Input("key", pg.Int8())
			out := q.DefineView(reg, q.ViewSpec[result]{Name: "entries"})
			probe := plan.DefineRead(out, "values", plan.Select(plan.Project(plan.Output(out, "ranks", kind), in.Expr())).
				From(src).Where(plan.Eq(id.Expr(), key.Expr())))
			run := authorizedEffectRunner(t, driverName, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				actual, err := rows.Bind(s).Find(ctx)
				require.NoError(t, err)
				require.Equal(t, []item{{ID: 1}, {ID: 2, Ranks: []*int32{}},
					{ID: 3, Ranks: []*int32{new(int32(0)), nil, new(int32(7))}}}, actual)
				populated := []*int32{new(int32(4)), nil}
				bound := plan.BindValue(in, populated)
				*populated[0], populated[1] = 8, new(int32(9))
				for range 2 {
					got, err := probe.Bind(s).One(ctx, bound, plan.BindValue(key, int64(1)))
					require.NoError(t, err)
					require.Equal(t, []*int32{new(int32(4)), nil}, got.Ranks)
				}
				for _, values := range [][]*int32{nil, {}} {
					got, err := probe.Bind(s).One(ctx, plan.BindValue(in, values), plan.BindValue(key, int64(1)))
					require.NoError(t, err)
					require.Equal(t, values, got.Ranks)
				}
				return nil
			}))
		})
	}
}
