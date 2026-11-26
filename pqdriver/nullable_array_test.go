package pqdriver

import (
	"database/sql"
	"database/sql/driver"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNullableArrayCodec(t *testing.T) {
	type label string
	type labels []*label
	var dst labels
	wrap, err := ArrayColumn(reflect.TypeFor[labels]())
	require.NoError(t, err)
	adapter := wrap(&dst)
	scanner, valuer := adapter.(sql.Scanner), adapter.(driver.Valuer)
	require.NoError(t, scanner.Scan(`{NULL,"NULL","a,b",""}`))
	require.Equal(t, labels{nil, new(label("NULL")), new(label("a,b")), new(label(""))}, dst)
	raw, err := valuer.Value()
	require.NoError(t, err)
	var second labels
	require.NoError(t, wrap(&second).(sql.Scanner).Scan(raw))
	require.Equal(t, dst, second)
	require.NoError(t, scanner.Scan("{}"))
	require.NotNil(t, dst)
	require.Empty(t, dst)
	require.NoError(t, scanner.Scan(nil))
	require.Nil(t, dst)
	var integers []*int16
	numbers, err := ArrayColumn(reflect.TypeFor[[]*int16]())
	require.NoError(t, err)
	require.NoError(t, numbers(&integers).(sql.Scanner).Scan("{3,NULL}"))
	require.Error(t, numbers(&integers).(sql.Scanner).Scan("{2,99999}"))
	require.Equal(t, []*int16{new(int16(3)), nil}, integers)
	_, err = ArrayColumn(reflect.TypeFor[[]*complex64]())
	require.Error(t, err)
}
