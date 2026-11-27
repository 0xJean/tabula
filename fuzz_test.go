package tabula

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/0xJean/tabula/internal/pgstmt"
	"github.com/0xJean/tabula/internal/planrt"
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
	f.Add("SELECT set_config('demo.x', '1', true)")
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
	f.Add("SET TRANSACTION READ WRITE")
	f.Add("SET TRANSACTION READ ONLY; SHOW transaction_read_only")
	f.Add("RESET default_transaction_read_only")
	f.Add(`SELECT $$SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY$$`)
	f.Add(`SELECT $$COMMIT$$, E'can\'t' /* ROLLBACK */`)
	f.Add("COMMIT")
	f.Add("SELECT 1; COMMIT AND CHAIN")
	f.Add("ROLLBACK WORK")
	f.Add("ROLLBACK WORK TO SAVEPOINT sp")
	f.Add("END TRANSACTION")
	f.Add("ABORT")
	f.Add("PREPARE TRANSACTION 'tx1'")
	f.Add("UPDATE pg_settings SET setting = 'x' WHERE name = 'scope.id'")
	f.Add(`UPDATE ONLY "pg_catalog" . pg_settings SET setting = $1`)
	f.Add("SELECT current_setting('scope.id', true)")
	f.Add(`SELECT $$SHOW scope.id$$; SHOW ALL`)
	f.Add("SELECT 1 AS pg_settings")
	f.Add("SELECT id pg_settings FROM items ORDER BY pg_settings")
	f.Add("SELECT 1 AS pg_settings FROM pg_catalog.pg_settings")
	f.Add("SELECT 'a;b' AS \"semi;colon\"; -- finished")
	f.Add("WITH x AS (VALUES (1)) SELECT * FROM x; /* end */")
	f.Add(`SELECT E'can\'t; finish', $note$one;two$note$`)
	f.Add("SELECT (1) /* unfinished")
	f.Add("WITH added AS (INSERT INTO items VALUES ($1) RETURNING id) SELECT id FROM added")
	f.Add("WITH added AS (INSERT INTO items VALUES ($1) RETURNING id) SELECT id FROM added; COMMIT")
	f.Add("WITH added AS (DELETE FROM items RETURNING id) SELECT id FROM added")
	methods, err := (StatementPolicy{Read: []string{"select", "with"}, Write: []string{"insert"}, RequireReturning: true}).Compile(false)
	if err != nil {
		f.Fatal(err)
	}
	reads, err := NewSessionGuard(SessionGuardConfig{VariablePrefixes: []string{"scope."}, ForbidVariableReads: true})
	if err != nil {
		f.Fatal(err)
	}
	signed, err := (StatementPolicy{Write: []string{"with", "insert"}, AllowAuthorizedWriteCTE: true}).Compile(false)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, query string) {
		authority, owner, call := signedPolicyCall(query, planrt.StatementEffects{WriteCTE: true, Writes: []planrt.WriteEffect{{Command: "INSERT"}}})
		if err := signed.CheckAuthorized(authority, owner, CallReturningEach, call); err == nil && GuardSingleStatement(query) != nil {
			t.Fatal("signed statement policy admitted stacked or incomplete SQL")
		}
		single := GuardSingleStatement(query)
		if single == nil && !errors.Is(GuardSingleStatement(query+"\n; SELECT 2"), ErrForbiddenStatement) {
			t.Fatal("single statement guard admitted a second command")
		}
		if err := methods.Check("Scan", query); err == nil && single != nil {
			t.Fatal("method policy admitted SQL denied by the single statement guard")
		}
		if err := GuardNoSessionEscape("UPDATE pg_settings SET setting = 'x';" + query); !errors.Is(err, ErrForbiddenStatement) {
			t.Fatalf("session guard allowed a pg_settings update prefix: %v", err)
		}
		if err := reads("SELECT current_setting('scope.id');" + query); !errors.Is(err, ErrForbiddenStatement) {
			t.Fatalf("variable-read guard allowed a protected read prefix: %v", err)
		}
		if err := GuardReadOnlyTransaction(query); err != nil && !errors.Is(err, ErrReadOnly) {
			t.Fatalf("read-only transaction guard returned %v for %q", err, query)
		}
		if err := GuardReadOnlyTransaction("SET TRANSACTION READ WRITE;" + query); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("read-only transaction guard returned %v for a mode change", err)
		}
		for _, txEnd := range []string{"COMMIT;", "ROLLBACK;", "END;", "ABORT;", "PREPARE TRANSACTION 'x';"} {
			if err := GuardNoSessionEscape(txEnd + query); !errors.Is(err, ErrForbiddenStatement) {
				t.Fatalf("session guard allowed transaction-ending prefix %q: %v", txEnd, err)
			}
			if err := configured(txEnd + query); !errors.Is(err, ErrForbiddenStatement) {
				t.Fatalf("configured guard allowed transaction-ending prefix %q: %v", txEnd, err)
			}
		}
		quoted := "SELECT '" + strings.ReplaceAll(query, "'", "''") + "'"
		if err := reads(quoted + " AS pg_settings"); err != nil {
			t.Fatalf("variable-read guard rejected a result label: %v", err)
		}
		if err := reads(quoted + " AS pg_settings FROM pg_catalog.pg_settings"); !errors.Is(err, ErrForbiddenStatement) {
			t.Fatalf("variable-read guard allowed a catalog read with a result label: %v", err)
		}
		if err := GuardReadOnlyTransaction(quoted); err != nil {
			t.Fatalf("read-only transaction guard rejected a quoted value: %v", err)
		}
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

func FuzzCompileQueryFragment(f *testing.F) {
	seeds := []string{
		"n.title ILIKE $1 OR n.code ILIKE $1",
		"n.created_at >= $1 AND n.created_at < $2",
		"n.meta ? $1 OR n.meta ?| $2",
		"n.tag = '$99' AND n.body = $q$keep $1$q$ AND n.x = $1",
		"-- comment\nn.active = true",
		"n.active = true -- no newline",
		"(n.a = $1",
		"n.a = $1)",
		"n.a = $1; DROP TABLE notes",
		"SET ROLE postgres",
		"SELECT set_config('demo.x', '1', true)",
		"n.a = $0",
		"n.a = $2",
		`n.a = E'it\'s $1' AND n.b = $1`,
		`/* nested /* $1 */ comment */ n.a = $1`,
		`/* unterminated`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, fragment string) {
		cf, err := compileSQLFragment(fragment, true)
		if err != nil {
			return
		}
		// Any compiled fragment must satisfy GuardNoSessionEscape and survive
		// arbitrary positive slot rebasing without error or panic.
		if guardErr := GuardNoSessionEscape(cf.raw); guardErr != nil {
			t.Fatalf("compiled fragment %q failed session guard: %v", cf.raw, guardErr)
		}
		rebased, err := pgstmt.RewriteParams(cf.raw, cf.spans, func(slot int) (int, error) {
			return slot + 5, nil
		})
		if err != nil {
			t.Fatalf("RewriteParams failed for compiled fragment %q: %v", cf.raw, err)
		}
		if _, err := pgstmt.ScanFragment(rebased); err != nil {
			t.Fatalf("rebased fragment %q failed ScanFragment: %v", rebased, err)
		}
	})
}
