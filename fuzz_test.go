package tabula

import (
	"slices"
	"strings"
	"testing"
)

func FuzzParseColumnTag(f *testing.F) {
	seeds := []string{
		"",
		"id,key",
		"id,key,serial",
		"tenant_id,tenant",
		"deleted_at,deleted",
		"version,version",
		"created_by,once",
		"password_hash,secret",
		"api_key,once,secret",
		"meta,json",
		"note,zero",
		"tags,array",
		"id,key,once",
		"bad,json,zero",
		"bad,array,json",
		"bad,unknown",
		",key",
		"a,b,c,d",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, tag string) {
		parsed, err := parseColumnTag(tag)
		if err != nil {
			return
		}
		if parsed.name == "" {
			t.Fatalf("empty name accepted for tag %q", tag)
		}
		if parsed.options["json"] && parsed.options["zero"] {
			t.Fatalf("json+zero accepted for tag %q", tag)
		}
		if parsed.options["array"] && (parsed.options["json"] || parsed.options["zero"]) {
			t.Fatalf("array conflict accepted for tag %q", tag)
		}
		if parsed.role != plain {
			for _, forbidden := range []string{"once", "json", "zero", "array", "secret"} {
				if parsed.options[forbidden] {
					t.Fatalf("role %s combined with %s for tag %q", parsed.role, forbidden, tag)
				}
			}
		}
	})
}

func FuzzFilterAnd(f *testing.F) {
	f.Add("code = ?", "btc", "eth")
	f.Add("payload ?? ? AND code = ?", "k", "v")
	f.Add("a = ? AND b = ? AND c = ?", "1", "2")
	f.Add("?????", "x", "y")
	f.Add("", "", "")
	f.Fuzz(func(t *testing.T, clause, v1, v2 string) {
		flt := NewFilter("fixed")
		flt.And(clause, v1, v2)
		args := flt.Args()
		if len(args) != 3 {
			t.Fatalf("expected 3 args, got %d", len(args))
		}
		// Mutating returned slice must not mutate Filter.
		args[0] = "mutated"
		if flt.Args()[0] != "fixed" {
			t.Fatalf("Args() did not return a defensive copy")
		}
		if w := flt.Where(); !strings.HasPrefix(w, " AND (") || !strings.HasSuffix(w, ")") {
			t.Fatalf("malformed Where(): %q", w)
		}
	})
}

func FuzzOrderSpecResolve(f *testing.F) {
	spec := MustOrderSpec(OrderSpec{
		Columns: map[string]string{
			"id":         "id",
			"created_at": "created_at",
			"code":       "code",
		},
		DefaultColumn:    "created_at",
		DefaultDirection: "DESC",
		TieBreaker:       "id ASC",
	})
	allowed := []string{
		"id ASC", "id DESC",
		"created_at ASC, id ASC", "created_at DESC, id ASC",
		"code ASC, id ASC", "code DESC, id ASC",
	}
	f.Add("code", "asc")
	f.Add("id; DROP TABLE users--", "DESC; --")
	f.Add("", "")
	f.Add("created_at\x00", "ASC\nDROP")
	f.Fuzz(func(t *testing.T, sortBy, dir string) {
		got := spec.Resolve(sortBy, dir)
		if !slices.Contains(allowed, got) {
			t.Fatalf("OrderSpec.Resolve(%q, %q) returned non-whitelisted SQL %q", sortBy, dir, got)
		}
	})
}
