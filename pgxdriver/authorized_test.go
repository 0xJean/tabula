package pgxdriver

import (
	"testing"

	"github.com/0xJean/tabula"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestAuthorizedBindingsContainOnlyValues(t *testing.T) {
	require.NoError(t, authorizedBindArgs([]any{nil, int64(4), "notes", []byte("value")}))
	for _, option := range []any{pgx.QueryExecModeExec, pgx.QueryResultFormats{0}, pgx.QueryResultFormatsByOID{25: 0}, pgx.NamedArgs{"value": 4}} {
		require.ErrorIs(t, authorizedBindArgs([]any{option}), tabula.ErrStatementAuthority)
	}
}
