package tabula

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func FuzzParseColumnTag(f *testing.F) {
	seeds := []string{
		"",
		"id,key",
		"id,key,serial",
		"tenant_id,scope",
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
	f.Add("code = ?", "alpha", "beta")
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

func FuzzGuardNoSessionEscape(f *testing.F) {
	configured, err := NewSessionGuard(SessionGuardConfig{VariablePrefixes: []string{"scope.", "session.", "local."}, ForbidRole: true})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(`SELECT $body$SET scope.id = 1$body$`)
	f.Add(`SET LOCAL "scope"."id" = 'one'`)
	f.Add("SELECT id FROM items WHERE id = $1")
	f.Add("SET LOCAL ROLE postgres")
	f.Add("SELECT set_config('app.x', '1', true)")
	f.Add("SELECT 'SET ROLE admin' /* RESET ROLE */")
	f.Add("/* nested /* comment */ still comment */ SET ROLE x")
	f.Add(`SELECT E'\''; SET ROLE postgres`)
	f.Add(`SELECT e'\''; RESET ALL`)
	f.Add("DISCARD ALL")
	f.Add("SELECT 1; /* reset connection */ DISCARD ALL")
	f.Add(`SELECT U&"lab\0065l" FROM items`)
	f.Add(`SELECT $body$DISCARD ALL; U&"label"$body$`)
	f.Add(`SELECT u & "mask" FROM items`)
	f.Add(`DO $$BEGIN NULL; END;$$`)
	f.Add(`SELECT E'one; two'; /* block */ DO LANGUAGE plpgsql $body$BEGIN NULL; END;$body$`)
	f.Add(`SELECT 'DO' AS "do"`)
	f.Add("INSERT INTO items (id) VALUES (1) ON CONFLICT DO NOTHING")
	f.Add("SET session.label = 'one'")
	f.Add("SET local.label TO 'two'")
	f.Add(`SET "local" /* namespace */ . "label" FROM CURRENT`)
	f.Add("SET LOCAL session.label TO DEFAULT")
	f.Add("SET SESSION other.label = 'three'")
	f.Fuzz(func(t *testing.T, query string) {
		sessionErr := GuardNoSessionEscape(query)
		readErr := GuardReadOnly(query)
		if !errors.Is(readErr, ErrReadOnly) && errors.Is(readErr, ErrForbiddenStatement) != errors.Is(sessionErr, ErrForbiddenStatement) {
			t.Fatalf("session and read-only guards disagree on %q: %v, %v", query, sessionErr, readErr)
		}
		protectedHead := strings.HasPrefix(query, "DO ") || strings.HasPrefix(query, "SET session.") || strings.HasPrefix(query, "SET local.")
		if err := configured(query); protectedHead && !errors.Is(err, ErrForbiddenStatement) {
			t.Fatalf("configured guard returned %v for %q, want ErrForbiddenStatement", err, query)
		}
	})
}
