package pgstmt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScanFragmentAndRewriteParams(t *testing.T) {
	t.Run("preserves literals, dollar quotes, escape strings and JSONB operators", func(t *testing.T) {
		q := `n.title ILIKE $1 OR n.meta ? $1 OR n.meta ?| $2 OR n.tag = '$99' OR n.body = $q$keep $88$q$ OR n.esc = E'it\'s $77' /* $66 */`
		spans, err := ScanFragment(q)
		require.NoError(t, err)
		got, err := RewriteParams(q, spans, func(slot int) (int, error) { return slot + 3, nil })
		require.NoError(t, err)
		want := `n.title ILIKE $4 OR n.meta ? $4 OR n.meta ?| $5 OR n.tag = '$99' OR n.body = $q$keep $88$q$ OR n.esc = E'it\'s $77' /* $66 */`
		require.Equal(t, want, got)
	})

	t.Run("accepts line comment followed by newline", func(t *testing.T) {
		q := "-- filter note\nn.rank >= $1"
		spans, err := ScanFragment(q)
		require.NoError(t, err)
		got, err := RewriteParams(q, spans, func(slot int) (int, error) { return slot + 1, nil })
		require.NoError(t, err)
		require.Equal(t, "-- filter note\nn.rank >= $2", got)
	})

	t.Run("rejects malformed syntax and statement separators", func(t *testing.T) {
		bad := []string{
			"a = $1; DROP TABLE x",
			"(a = $1",
			"a = $1)",
			")(a = $1)",
			"a = 'unterminated",
			`a = E'unterminated\`,
			`a = "unterminated`,
			"a = $tag$unterminated",
			"/* nested /* comment */",
			"a = $1 -- trailing comment without newline",
			"a = $0",
			"a = $99999",
		}
		for _, q := range bad {
			_, err := ScanFragment(q)
			require.Error(t, err, q)
		}
	})
}
