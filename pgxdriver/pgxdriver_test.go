package pgxdriver

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

func TestSQLStateHelpers(t *testing.T) {
	require.Equal(t, "23505", SQLState(fmt.Errorf("wrap: %w", &pgconn.PgError{Code: "23505"})))
	require.Equal(t, "", SQLState(errors.New("plain")))
	require.True(t, IsUniqueViolation(&pgconn.PgError{Code: "23505"}))
	require.True(t, IsForeignKeyViolation(&pgconn.PgError{Code: "23503"}))
	require.True(t, IsCheckViolation(&pgconn.PgError{Code: "23514"}))
	require.True(t, IsNotNullViolation(&pgconn.PgError{Code: "23502"}))
	require.True(t, IsSerializationFailure(&pgconn.PgError{Code: "40001"}))
	require.True(t, IsDeadlock(&pgconn.PgError{Code: "40P01"}))
	require.True(t, IsQueryCanceled(&pgconn.PgError{Code: "57014"}))
	require.True(t, IsNumericOutOfRange(&pgconn.PgError{Code: "22003"}))
	require.True(t, IsRejectedValue(&pgconn.PgError{Code: "22P02"}))
	require.False(t, IsRejectedValue(&pgconn.PgError{Code: "23505"}))
}

func TestArrayColumn(t *testing.T) {
	// Plain []string passes pointer straight through for pgx native codec.
	wrapPlain, err := ArrayColumn(reflect.TypeFor[[]string]())
	require.NoError(t, err)
	var plain []string
	require.Equal(t, &plain, wrapPlain(&plain))

	// Named string slice converts cleanly.
	type tag string
	wrapNamed, err := ArrayColumn(reflect.TypeFor[[]tag]())
	require.NoError(t, err)
	var tags []tag
	conv := wrapNamed(&tags)
	require.NoError(t, conv.(interface{ Scan(any) error }).Scan([]string{"a", "b"}))
	require.Equal(t, []tag{"a", "b"}, tags)
	v, err := conv.(driver.Valuer).Value()
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, v)

	// Named int64 slice converts cleanly.
	type id int64
	wrapID, err := ArrayColumn(reflect.TypeFor[[]id]())
	require.NoError(t, err)
	var ids []id
	convID := wrapID(&ids)
	require.NoError(t, convID.(interface{ Scan(any) error }).Scan([]int64{10, 20}))
	require.Equal(t, []id{10, 20}, ids)
	v, err = convID.(driver.Valuer).Value()
	require.NoError(t, err)
	require.Equal(t, []int64{10, 20}, v)

	_, err = ArrayColumn(reflect.TypeFor[int]())
	require.ErrorContains(t, err, "slice type")
	_, err = ArrayColumn(reflect.TypeFor[[]bool]())
	require.ErrorContains(t, err, "supports []~string or []~int64")
}
