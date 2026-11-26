package pgtype

import (
	"encoding/json"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

// Preserve the function types published by the root package.
var (
	_ func(any) any                   = tabula.JSON
	_ func(int) tabula.PGType[string] = tabula.Varchar
)

func TestVarcharDeclaration(t *testing.T) {
	for _, lengths := range [][]int{{0}, {-1}, {10_485_761}, {1, 2}} {
		require.Panics(t, func() { Varchar(lengths...) })
	}
	require.NotPanics(t, func() { Varchar() })
	require.NotPanics(t, func() { Varchar(10_485_760) })
}

func TestJSONStorage(t *testing.T) {
	raw := json.RawMessage(`{ "field" : 1 }`)
	value, err := encodeJSON(raw)
	require.NoError(t, err)
	raw[0] = '['
	require.Equal(t, `{ "field" : 1 }`, value)
	items := map[string]int{"field": 1}
	value, err = encodeJSON(items)
	require.NoError(t, err)
	items["field"]++
	require.Equal(t, `{"field":1}`, value)
	for _, invalid := range []any{json.RawMessage{}, raw, make(chan int)} {
		value, err := encodeJSON(invalid)
		require.Error(t, err)
		require.Nil(t, value)
	}
}
