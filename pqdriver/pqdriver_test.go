package pqdriver

import (
	"database/sql/driver"
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakePGError string

func (e fakePGError) Error() string    { return "pg " + string(e) }
func (e fakePGError) SQLState() string { return string(e) }

type rowScanner interface{ Scan(any) error }

func TestSQLStateClassifiers(t *testing.T) {
	require.Equal(t, "23505", SQLState(fmt.Errorf("wrap: %w", fakePGError("23505"))))
	require.Equal(t, "", SQLState(fmt.Errorf("plain")))
	require.True(t, IsUniqueViolation(fakePGError("23505")))
	require.False(t, IsUniqueViolation(fakePGError("23503")))

	for _, code := range []string{"22001", "22003", "23502", "23514", "22P02"} {
		require.True(t, IsRejectedValue(fakePGError(code)), code)
	}
	require.False(t, IsRejectedValue(fakePGError("23505")))
	require.True(t, IsNumericOutOfRange(fakePGError("22003")))
	require.False(t, IsNumericOutOfRange(fakePGError("23514")))
}

func TestArrayConverters(t *testing.T) {
	type roleTag string
	var tags []roleTag
	tc := TextArray(&tags)
	require.NoError(t, tc.(rowScanner).Scan(`{"a","b"}`))
	require.Equal(t, []roleTag{"a", "b"}, tags)
	v, err := tc.(driver.Valuer).Value()
	require.NoError(t, err)
	require.Equal(t, `{"a","b"}`, v)
	require.NoError(t, tc.(rowScanner).Scan(nil))
	require.Nil(t, tags)

	type idCode int64
	var ids []idCode
	ic := Int64Array(&ids)
	require.NoError(t, ic.(rowScanner).Scan(`{1,2}`))
	require.Equal(t, []idCode{1, 2}, ids)
	v, err = ic.(driver.Valuer).Value()
	require.NoError(t, err)
	require.Equal(t, `{1,2}`, v)
	require.NoError(t, ic.(rowScanner).Scan(nil))
	require.Nil(t, ids)
}

func TestArrayColumnReflectionResolver(t *testing.T) {
	type factor string
	wrapStr, err := ArrayColumn(reflect.TypeFor[[]factor]())
	require.NoError(t, err)
	var gotStr []factor
	convStr := wrapStr(&gotStr)
	require.NoError(t, convStr.(rowScanner).Scan(`{"pin","totp"}`))
	require.Equal(t, []factor{"pin", "totp"}, gotStr)
	v, err := convStr.(driver.Valuer).Value()
	require.NoError(t, err)
	require.Equal(t, `{"pin","totp"}`, v)
	require.NoError(t, convStr.(rowScanner).Scan(nil))
	require.Nil(t, gotStr)

	type num int64
	wrapInt, err := ArrayColumn(reflect.TypeFor[[]num]())
	require.NoError(t, err)
	var gotInt []num
	convInt := wrapInt(&gotInt)
	require.NoError(t, convInt.(rowScanner).Scan(`{7,8}`))
	require.Equal(t, []num{7, 8}, gotInt)
	v, err = convInt.(driver.Valuer).Value()
	require.NoError(t, err)
	require.Equal(t, `{7,8}`, v)

	_, err = ArrayColumn(reflect.TypeFor[string]())
	require.EqualError(t, err, "array option requires a slice type, got string")
	_, err = ArrayColumn(reflect.TypeFor[[]bool]())
	require.EqualError(t, err, "array option supports []~string or []~int64, got []bool")
}
