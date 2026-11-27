package pgstmt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRewriteNamedSQL(t *testing.T) {
	t.Run("rewrites named parameters and reuses slot numbers", func(t *testing.T) {
		got, names, err := RewriteNamedSQL(
			"SELECT id, name FROM public.items WHERE (code ILIKE @q OR name ILIKE @q) AND status = @status AND id = @id::bigint;",
			false,
		)
		require.NoError(t, err)
		require.Equal(t, []string{"q", "status", "id"}, names)
		require.Equal(t,
			"SELECT id, name FROM public.items WHERE (code ILIKE $1 OR name ILIKE $1) AND status = $2 AND id = $3::bigint",
			got,
		)
	})

	t.Run("ignores comments, quotes, escape strings, dollar quotes and quoted identifiers", func(t *testing.T) {
		in := strings.Join([]string{
			"-- @ignored_line_comment",
			"/* outer /* @nested_comment */ @still_comment */",
			"SELECT id, \"@quoted_col\" AS label",
			"FROM public.items",
			"WHERE note = '@in_single'",
			"  AND esc = E'it\\'s @in_escape \\\\ @also_escape'",
			"  AND raw = $$keep @in_dollar and $1$$",
			"  AND tagged = $tag$keep @in_tag and $2$tag$",
			"  AND code = @real_code",
		}, "\n")
		got, names, err := RewriteNamedSQL(in, false)
		require.NoError(t, err)
		require.Equal(t, []string{"real_code"}, names)
		require.Contains(t, got, "-- @ignored_line_comment")
		require.Contains(t, got, "/* outer /* @nested_comment */ @still_comment */")
		require.Contains(t, got, `"@quoted_col"`)
		require.Contains(t, got, `'@in_single'`)
		require.Contains(t, got, `E'it\'s @in_escape \\ @also_escape'`)
		require.Contains(t, got, `$$keep @in_dollar and $1$$`)
		require.Contains(t, got, `$tag$keep @in_tag and $2$tag$`)
		require.True(t, strings.HasSuffix(got, "AND code = $1"))
	})

	t.Run("preserves PostgreSQL @ operators and non-boundary @", func(t *testing.T) {
		cases := []struct {
			in, want string
			names    []string
		}{
			{
				in:    "SELECT id FROM items WHERE payload @> @filter AND tags <@ @allowed AND doc @@ @query",
				want:  "SELECT id FROM items WHERE payload @> $1 AND tags <@ $2 AND doc @@ $3",
				names: []string{"filter", "allowed", "query"},
			},
			{
				in:    "SELECT @ -5 AS abs_val, @(rank) AS abs_col FROM items WHERE tags<@other AND doc@@tsq AND payload@>meta",
				want:  "SELECT @ -5 AS abs_val, @(rank) AS abs_col FROM items WHERE tags<@other AND doc@@tsq AND payload@>meta",
				names: nil,
			},
			{
				in:    "SELECT id FROM items WHERE id=@id AND arr[1:@n] = ARRAY[@a,@b] AND x = @id+@n",
				want:  "SELECT id FROM items WHERE id=$1 AND arr[1:$2] = ARRAY[$3,$4] AND x = $1+$2",
				names: []string{"id", "n", "a", "b"},
			},
			{
				in:    "SELECT foo@bar, 1@bar, x::@bar, a.@bar, @a@b FROM items WHERE id = @ok",
				want:  "SELECT foo@bar, 1@bar, x::@bar, a.@bar, @a@b FROM items WHERE id = $1",
				names: []string{"ok"},
			},
			{
				in:    "SELECT id FROM items WHERE data->@k = @v AND data->>@k2 = @v",
				want:  "SELECT id FROM items WHERE data->$1 = $2 AND data->>$3 = $2",
				names: []string{"k", "v", "k2"},
			},
		}
		for _, tc := range cases {
			got, names, err := RewriteNamedSQL(tc.in, false)
			require.NoError(t, err, tc.in)
			require.Equal(t, tc.want, got, tc.in)
			require.Equal(t, tc.names, names, tc.in)
		}
	})

	t.Run("rejects positional placeholders, stacked statements, and lexical bypasses", func(t *testing.T) {
		bad := []string{
			"",
			"   -- only comment\n",
			"SELECT id FROM items WHERE id = $1",
			"SELECT id FROM items WHERE id = @id AND rank = $2",
			"SELECT id FROM items WHERE id = $0",
			"SELECT id FROM items; DROP TABLE items",
			"SELECT id FROM items;;",
			"; SELECT id FROM items",
			"(SELECT id FROM items;)",
			"SELECT id FROM items WHERE id = @id; -- ok\nSELECT 2",
			"SELECT 'unterminated @id",
			`SELECT E'unterminated @id\`,
			`SELECT 'portable\' || ' @id'`,
			`SELECT "unterminated @id`,
			"SELECT $$unterminated @id",
			"SELECT $tag$unterminated @id",
			"SELECT /* outer /* inner */ @id",
			"SELECT (id FROM items WHERE id = @id",
			"SELECT id) FROM items WHERE id = @id",
			"SELECT id FROM items\x00 WHERE id = @id",
			string([]byte{'S', 'E', 'L', 'E', 'C', 'T', ' ', 0xff}),
		}
		for _, q := range bad {
			_, _, err := RewriteNamedSQL(q, false)
			require.Error(t, err, q)
		}
	})

	t.Run("fragment mode additionally rejects semicolons and trailing line comments", func(t *testing.T) {
		_, _, err := RewriteNamedSQL("status = @status -- trailing", true)
		require.Error(t, err)
		_, _, err = RewriteNamedSQL("status = @status;", true)
		require.Error(t, err)
		got, names, err := RewriteNamedSQL("status = @status -- ok\n", true)
		require.NoError(t, err)
		require.Equal(t, "status = $1 -- ok\n", got)
		require.Equal(t, []string{"status"}, names)
	})
}

func FuzzRewriteNamedSQL(f *testing.F) {
	seeds := []string{
		"SELECT id FROM items WHERE id = @id",
		"SELECT id FROM items WHERE code = '@id' AND note = E'\\' @id' AND raw = $$@id $1$$ AND x = @x",
		"SELECT id FROM items WHERE payload @> @p AND tags <@ @t AND doc @@ @q AND v = @ -5",
		"SELECT id FROM items WHERE id = $1",
		"SELECT id FROM items; SET ROLE admin",
		"SELECT 'a\\' || 'b'",
		"SELECT /* /* @a */ @b */ @c FROM items",
		"SELECT id FROM items -- @comment",
		"SELECT id FROM items WHERE a = @a AND b = @a + @b",
		`SELECT "@a" FROM "items" WHERE "col" = @a`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		for _, fragment := range []bool{false, true} {
			rewritten, names, err := RewriteNamedSQL(sql, fragment)
			if err != nil {
				continue
			}
			// Rewritten SQL must never retain invalid UTF-8 or NUL bytes.
			if strings.IndexByte(rewritten, 0) >= 0 {
				t.Fatalf("rewritten SQL contains NUL byte: %q", rewritten)
			}
			// Every discovered name must be a valid ASCII identifier.
			seen := make(map[string]bool, len(names))
			for _, name := range names {
				if !ValidNamedParam(name) {
					t.Fatalf("invalid named param %q from %q", name, sql)
				}
				if seen[name] {
					t.Fatalf("duplicate param %q in order list from %q", name, sql)
				}
				seen[name] = true
			}
			// Re-scanning rewritten SQL with ScanFragment (when not ending with line comment)
			// must agree on balanced structure and produce only slots within 1..len(names).
			if fragment {
				spans, scanErr := ScanFragment(rewritten)
				if scanErr != nil {
					t.Fatalf("ScanFragment rejected rewritten fragment %q (from %q): %v", rewritten, sql, scanErr)
				}
				for _, sp := range spans {
					if sp.Kind == Param && (sp.ParamNum < 1 || sp.ParamNum > len(names)) {
						t.Fatalf("out-of-range param $%d (max %d) in %q (from %q)", sp.ParamNum, len(names), rewritten, sql)
					}
				}
			}
		}
	})
}
