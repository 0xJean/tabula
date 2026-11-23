package pgxdriver

import (
	"errors"
	"fmt"
	"github.com/0xJean/tabula"
	"github.com/jackc/pgx/v5/pgtype"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

func TestSQLStateHelpers(t *testing.T) {
	require.Equal(t, "23505", tabula.SQLState(fmt.Errorf("wrap: %w", &pgconn.PgError{Code: "23505"})))
	require.Equal(t, "", tabula.SQLState(errors.New("plain")))
	require.True(t, tabula.IsUniqueViolation(&pgconn.PgError{Code: "23505"}))
	require.True(t, tabula.IsForeignKeyViolation(&pgconn.PgError{Code: "23503"}))
	require.True(t, tabula.IsCheckViolation(&pgconn.PgError{Code: "23514"}))
	require.True(t, tabula.IsNotNullViolation(&pgconn.PgError{Code: "23502"}))
	require.True(t, tabula.IsSerializationFailure(&pgconn.PgError{Code: "40001"}))
	require.True(t, tabula.IsDeadlock(&pgconn.PgError{Code: "40P01"}))
	require.True(t, tabula.IsQueryCanceled(&pgconn.PgError{Code: "57014"}))
	require.True(t, tabula.IsNumericOutOfRange(&pgconn.PgError{Code: "22003"}))
	require.True(t, tabula.IsRejectedValue(&pgconn.PgError{Code: "22P02"}))
	require.False(t, tabula.IsRejectedValue(&pgconn.PgError{Code: "23505"}))
}

func TestArrayColumn(t *testing.T) {
	// Plain []string passes pointer straight through for pgx native codec.
	wrapPlain, err := ArrayColumn(reflect.TypeFor[[]string]())
	require.NoError(t, err)
	var plain []string
	require.Equal(t, &plain, wrapPlain(&plain))

	type tag string
	wrap, err := ArrayColumn(reflect.TypeFor[[]tag]())
	require.NoError(t, err)
	tm := pgtype.NewMap()
	want := []tag{"a", "b"}
	buf, err := tm.Encode(pgtype.TextArrayOID, pgtype.BinaryFormatCode, wrap(&want), nil)
	require.NoError(t, err)
	var got []tag
	require.NoError(t, tm.Scan(pgtype.TextArrayOID, pgtype.BinaryFormatCode, buf, wrap(&got)))
	require.Equal(t, want, got)
	_, err = ArrayColumn(reflect.TypeFor[int]())
	require.ErrorContains(t, err, "slice type")
	_, err = ArrayColumn(reflect.TypeFor[[]int16]())
	require.ErrorContains(t, err, "unsupported array element type")
}
