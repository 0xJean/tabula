package stdsql

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestQuoteIdent(t *testing.T) {
	require.Equal(t, `"sp_1"`, quoteIdent("sp_1"))
	require.Equal(t, `"a""b"`, quoteIdent(`a"b`))
}
