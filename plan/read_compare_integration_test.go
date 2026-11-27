//go:build integration

package plan

import (
	"github.com/0xJean/tabula"

	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadCursorOperatorCatalog(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	for _, name := range []string{"bool", "int2", "int4", "int8", "oid", "float4", "float8", "numeric", "text", "varchar", "bpchar", "bytea", "uuid", "date", "time", "timetz", "timestamp", "timestamptz", "interval", "jsonb", "inet", "cidr", "macaddr", "macaddr8"} {
		t.Run(name, func(t *testing.T) {
			typ := tabula.CatalogType[string]("pg_catalog", name, nil)
			for _, kind := range []catalogType{typeID(typ), typeID(ArrayOf(typ))} {
				for operation, symbol := range map[string]string{"gt": ">", "lt": "<", "eq": "="} {
					compiler := &typedCompiler{}
					require.NoError(t, requireReadComparison(compiler, kind, operation))
					require.Len(t, compiler.functions, 1)
					for signature := range compiler.functions {
						var matches bool
						err := db.QueryRowContext(t.Context(), `SELECT EXISTS (
SELECT FROM pg_catalog.pg_operator o JOIN pg_catalog.pg_namespace n ON n.oid = o.oprnamespace
WHERE n.nspname = 'pg_catalog' AND o.oprname = $2 AND o.oprcode = pg_catalog.to_regprocedure($1))`, signature, symbol).Scan(&matches)
						require.NoError(t, err)
						require.True(t, matches, "%s must implement pg_catalog.%s", signature, symbol)
					}
				}
			}
		})
	}
}

func TestReadAggregateRepresentativeTies(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	var numberForward, numberReverse, floatForward, floatReverse string
	err = db.QueryRowContext(t.Context(), `SELECT
(SELECT min(v)::text FROM (SELECT v FROM (VALUES (1.0::numeric),(1.00::numeric)) n(v) ORDER BY scale(v)) a),
(SELECT min(v)::text FROM (SELECT v FROM (VALUES (1.0::numeric),(1.00::numeric)) n(v) ORDER BY scale(v) DESC) a),
(SELECT min(v)::text FROM (VALUES ('-0'::float8),('0'::float8)) n(v)),
(SELECT min(v)::text FROM (VALUES ('0'::float8),('-0'::float8)) n(v))`).Scan(&numberForward, &numberReverse, &floatForward, &floatReverse)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"1.0", "1.00"}, []string{numberForward, numberReverse})
	require.ElementsMatch(t, []string{"-0", "0"}, []string{floatForward, floatReverse})
}
