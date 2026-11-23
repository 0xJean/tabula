package pgstmt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTokensEscapeStrings(t *testing.T) {
	// E'\'' closes after the escaped quote; a stacked statement must be tokenized.
	toks := Tokens(`SELECT E'\''; SET ROLE postgres`)
	require.Equal(t, []string{"select", ";", "set", "role", "postgres"}, toks)
	require.Equal(t, "SET ROLE", SessionChange(toks))

	toksLower := Tokens(`SELECT e'\''; RESET ALL`)
	require.Equal(t, []string{"select", ";", "reset", "all"}, toksLower)
	require.Equal(t, "RESET ALL", SessionChange(toksLower))

	// Escaped backslash before closing quote ends the literal cleanly.
	toksSlash := Tokens(`SELECT E'\\'; SET SESSION AUTHORIZATION DEFAULT`)
	require.Equal(t, "SET SESSION AUTHORIZATION", SessionChange(toksSlash))

	// Doubled quote inside E'...' stays inside the literal; stacked statement after closing quote is still caught.
	toksDoubled := Tokens(`SELECT E'a''b'; SET LOCAL ROLE admin`)
	require.Equal(t, "SET ROLE", SessionChange(toksDoubled))

	// Benign E'...' containing escaped quote and keywords inside the literal is allowed.
	toksBenign := Tokens(`SELECT E'don\'t; SET ROLE postgres' AS msg`)
	require.Equal(t, []string{"select", "as", "msg"}, toksBenign)
	require.Empty(t, SessionChange(toksBenign))

	// Unclosed E'...' ending in a trailing backslash does not panic.
	require.NotPanics(t, func() {
		_ = Tokens(`SELECT E'unclosed\`)
	})

	// Identifier ending in 'e' followed by string literal is still tokenized normally.
	toksIdent := Tokens(`SELECT name, 'SET ROLE x' FROM items`)
	require.Equal(t, []string{"select", "name", "from", "items"}, toksIdent)
	require.Empty(t, SessionChange(toksIdent))
}
