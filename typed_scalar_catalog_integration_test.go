//go:build integration

package tabula

import (
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise every admitted pair against the real parser, including pairs whose
// Go representations are identical. NULL operands isolate signature resolution
// from input values; dual-driver tests separately cover values and SQLSTATE.
func TestScalarCatalogSignatures(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	check := func(value Scalar) {
		t.Helper()
		query, c := renderScalar(t, value)
		var matches bool
		err := db.QueryRowContext(t.Context(), "SELECT pg_typeof("+query+") = $1::regtype", value.scalarOperand().kind.qualified()).Scan(&matches)
		require.NoError(t, err, query)
		require.True(t, matches, query)
		for signature := range c.functions {
			var exists bool
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT to_regprocedure($1) IS NOT NULL", signature).Scan(&exists))
			require.True(t, exists, signature)
		}
	}
	types := []string{"bool", "int2", "int4", "int8", "float4", "float8", "numeric", "oid", "text", "varchar", "bpchar", "bytea", "uuid",
		"json", "jsonb", "date", "time", "timetz", "timestamp", "timestamptz", "interval", "inet", "cidr"}
	for _, a := range types {
		left := CatalogType[any]("pg_catalog", a, nil)
		for _, b := range types {
			right := CatalogType[any]("pg_catalog", b, nil)
			if _, ok := scalarCastSignature(left.id, right.id); ok {
				check(Cast(right, TypedNull(left)))
				check(Cast(ArrayOf(right), TypedNull(ArrayOf(left))))
			}
			for _, op := range []string{"+", "-", "*", "/"} {
				if result := arithmeticResult(op, left.id, right.id); result != "" {
					check(arithmetic(CatalogType[any]("pg_catalog", result, nil), op, TypedNull(left).scalarOperand(), TypedNull(right).scalarOperand()))
				}
			}
		}
		if comparableScalar(left.id, false) {
			check(NullIf(TypedNull(left), TypedNull(left)))
			check(NotEqualAll(TypedNull(left), TypedNull(ArrayOf(left))))
			check(Greatest(TypedNull(left), TypedNull(left)))
			check(Least(TypedNull(left), TypedNull(left)))
		}
	}
}
