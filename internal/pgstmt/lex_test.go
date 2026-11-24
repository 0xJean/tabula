package pgstmt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLexEscapeStrings(t *testing.T) {
	for _, tc := range []struct{ sql, value string }{
		{`E'\''`, `\'`},
		{`e'\''`, `\'`},
		{`E'\\'`, `\\`},
		{`E'a''b'`, "a'b"},
		{`E'don\'t; SET ROLE postgres'`, `don\'t; SET ROLE postgres`},
	} {
		require.Equal(t, []Token{{Literal, tc.value}, {Symbol, ";"}, {Word, "reset"}, {Word, "all"}},
			Lex(tc.sql+"; RESET ALL"), tc.sql)
	}
	require.Equal(t, []Token{{Literal, `unclosed\`}}, Lex(`E'unclosed\`))
	require.Equal(t, []Token{{Word, "name"}, {Symbol, ","}, {Literal, "SET ROLE x"}}, Lex(`name, 'SET ROLE x'`))
}

func TestLexUnicodeIdentifierPrefix(t *testing.T) {
	require.Equal(t, []Token{{Symbol, "u&"}, {Quoted, "label"}}, Lex(`U&"label"`))
	for _, expr := range []string{`u & "mask"`, `u/* comment */&"mask"`, `u& "mask"`} {
		require.Equal(t, []Token{{Word, "u"}, {Symbol, "&"}, {Quoted, "mask"}}, Lex(expr), expr)
	}
}

func TestDataStatementBoundaries(t *testing.T) {
	for _, q := range []string{`SELECT $1, $$one; two$$;`, `(SELECT 'a''b')`, `/* outer /* inner */ */ UPDATE "MiXeD" SET x=1`} {
		_, ok := DML(q)
		require.True(t, ok, q)
	}
	for _, q := range []string{"", "(", "RESET ALL", "SELECT 1; SELECT 2"} {
		_, ok := DML(q)
		require.False(t, ok, q)
	}
	tokens := Lex(`SELECT "With.Dot", Äbc, $tag$note;$tag$ FROM items`)
	require.Contains(t, tokens, Token{Quoted, "With.Dot"})
	require.Contains(t, tokens, Token{Word, "Äbc"})
	require.Contains(t, tokens, Token{Literal, "note;"})
}
