package tabula

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGuardReadOnlyTransaction(t *testing.T) {
	for _, query := range []string{
		"SET TRANSACTION READ WRITE",
		"SET TRANSACTION READ ONLY",
		"SET TRANSACTION ISOLATION LEVEL REPEATABLE READ",
		"SET TRANSACTION ISOLATION LEVEL READ COMMITTED, READ WRITE",
		`SET LOCAL "transaction_read_only" = off`,
		"RESET transaction_read_only",
		`SET SESSION "default_transaction_read_only" TO DEFAULT`,
		"RESET default_transaction_read_only",
		"SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED, READ ONLY",
		"RESET ALL", "DISCARD ALL",
		"SELECT pg_catalog.set_config($1, $2, true)",
		"SELECT pg_catalog.set_config('default_transaction_read_only', 'on', true)",
		"DO $$BEGIN NULL; END;$$",
		"SELECT pg_catalog.set_config('transaction_read_only', 'off', true)",
		"SELECT 'safe'; /* end */ COMMIT",
		"END WORK", "ABORT", "ROLLBACK WORK", "PREPARE TRANSACTION 'name'",
	} {
		err := GuardReadOnlyTransaction(query)
		require.ErrorIs(t, err, ErrReadOnly, query)
		require.ErrorIs(t, err, ErrForbiddenStatement, query)
	}
	commitErr := GuardReadOnlyTransaction("COMMIT")
	require.ErrorContains(t, commitErr, "transaction control (commit)")
	require.NotContains(t, commitErr.Error(), "requires a writable session")
	for _, query := range []string{
		"SHOW transaction_read_only", "SHOW default_transaction_read_only",
		"SET LOCAL statement_timeout = '1s'", "SET LOCAL transaction_read_only.label = 'one'",
		"SELECT pg_catalog.set_config('application_name', 'reader', true)",
		"SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED; SELECT 'read write'",
		"SAVEPOINT part; ROLLBACK TRANSACTION TO SAVEPOINT part; RELEASE SAVEPOINT part",
		"PREPARE lookup AS SELECT 1", "PREPARE \"transaction\" AS SELECT 1",
		`SELECT 'SET TRANSACTION READ WRITE', $$COMMIT; END$$, E'can\'t', 1 AS "rollback"`,
		"SELECT 1 /* SET TRANSACTION READ WRITE */ -- COMMIT",
	} {
		require.NoError(t, GuardReadOnlyTransaction(query), query)
	}
}

func TestConfiguredSessionGuard(t *testing.T) {
	prefixes := []string{"Scope."}
	guard, err := NewSessionGuard(SessionGuardConfig{VariablePrefixes: prefixes, ForbidRole: true, ForbidSessionAuthorization: true})
	require.NoError(t, err)
	prefixes[0] = "unrelated."
	for _, q := range []string{
		"SET scope.id TO 'a'", "SET LOCAL scope.id = 'a'", "RESET scope.id", `SET SESSION "scope"."id" = 'a'`, "RESET ALL", "DISCARD ALL",
		"SET ROLE reader", "SET SESSION AUTHORIZATION DEFAULT", "SELECT pg_catalog.set_config('role','reader',true)",
		"SELECT set_config('session_authorization','reader',true)", "SELECT set_config($1,$2,true)", "SELECT set_config('scope.id','a',true)",
		`SELECT $tag$RESET ALL$tag$; /* scope */ RESET "scope"."id"`,
		"COMMIT", "COMMIT WORK", "COMMIT AND CHAIN", "COMMIT PREPARED 'x'",
		"END", "END TRANSACTION", "ABORT", "ABORT WORK",
		"ROLLBACK", "ROLLBACK WORK", "ROLLBACK TRANSACTION", "ROLLBACK AND CHAIN", "ROLLBACK PREPARED 'x'",
		"PREPARE TRANSACTION 'tx1'", "SELECT 1; /* comment */ COMMIT",
	} {
		require.ErrorIs(t, guard(q), ErrForbiddenStatement, q)
	}
	for _, q := range []string{
		"COMMIT", "END", "ABORT", "ROLLBACK", "ROLLBACK WORK", "PREPARE TRANSACTION 'tx1'", "SELECT 1; COMMIT",
	} {
		require.ErrorIs(t, GuardNoSessionEscape(q), ErrForbiddenStatement, q)
	}
	for _, q := range []string{
		"SELECT 'RESET ALL'", "SELECT 'COMMIT'", `SELECT $$ROLLBACK; END; ABORT$$`,
		"SAVEPOINT sp; ROLLBACK TO SAVEPOINT sp; RELEASE SAVEPOINT sp",
		"ROLLBACK WORK TO sp", "ROLLBACK TRANSACTION TO SAVEPOINT sp",
		"PREPARE lookup AS SELECT 1", `PREPARE "transaction" AS SELECT 1`,
		"SET LOCAL unrelated.id = 'a'", "RESET timezone", "UPDATE items SET role=$1",
		"SELECT set_config('unrelated.id','a',true)", `SELECT $$set_config('scope.id','a',true)$$`,
	} {
		require.NoError(t, guard(q), q)
	}
	strict, err := NewSessionGuard(SessionGuardConfig{ForbidSetConfig: true})
	require.NoError(t, err)
	require.ErrorIs(t, strict("SELECT set_config('timezone','UTC',true)"), ErrForbiddenStatement)
	require.NoError(t, strict("SET LOCAL ROLE reader"))
	g, err := NewSessionGuard(SessionGuardConfig{})
	require.NoError(t, err)
	require.NoError(t, g("RESET ALL"))
	_, err = NewSessionGuard(SessionGuardConfig{VariablePrefixes: []string{" "}})
	require.Error(t, err)
}

func TestSessionGuardAnonymousBlock(t *testing.T) {
	const block = `/* setup */ DO LANGUAGE plpgsql 'BEGIN NULL; END;'`
	for _, cfg := range []SessionGuardConfig{
		{VariablePrefixes: []string{"scope."}},
		{ForbidRole: true},
		{ForbidSessionAuthorization: true},
		{ForbidSetConfig: true},
	} {
		guard, err := NewSessionGuard(cfg)
		require.NoError(t, err)
		require.ErrorIs(t, guard(block), ErrForbiddenStatement, "config %+v", cfg)
	}
	guard, err := NewSessionGuard(SessionGuardConfig{})
	require.NoError(t, err)
	require.NoError(t, guard(block))
}

func TestSessionGuardKeywordNamespaces(t *testing.T) {
	guard, err := NewSessionGuard(SessionGuardConfig{VariablePrefixes: []string{"SESSION.", "local."}})
	require.NoError(t, err)
	for _, q := range []string{
		"SET session.label = 'one'", "SET local.label TO 'two'",
		`SELECT 1; SET LoCaL /* namespace */ . "label" TO DEFAULT`,
		`SET "session"."label" = 'one'`, `SET "local.label" FROM CURRENT`,
		"SET LOCAL session.label = 'one'", "SET SESSION local.label FROM CURRENT",
		"RESET session.label", `RESET "local"."label"`,
	} {
		require.ErrorIs(t, guard(q), ErrForbiddenStatement, q)
		require.NoError(t, GuardNoSessionEscape(q), "default guard has no protected GUC prefixes: %s", q)
	}
	for _, q := range []string{
		"SET SESSION other.label = 'one'", "SET LOCAL other.label = 'two'",
		"SET session_extra.label TO DEFAULT", "SET local_extra.label FROM CURRENT",
		"SET SESSION AUTHORIZATION DEFAULT",
	} {
		require.NoError(t, guard(q), q)
	}
}

func TestSessionGuardSettingsCatalogUpdate(t *testing.T) {
	scoped, err := NewSessionGuard(SessionGuardConfig{VariablePrefixes: []string{"scope."}})
	require.NoError(t, err)
	strict, err := NewSessionGuard(SessionGuardConfig{ForbidSetConfig: true})
	require.NoError(t, err)
	for _, q := range []string{
		"UPDATE pg_settings SET setting = 'x' WHERE name = 'scope.id'",
		"update PG_SETTINGS set setting = 'x' where name = 'scope.' || 'id'",
		"UPDATE ONLY pg_catalog.pg_settings SET setting = $1 WHERE name = $2",
		`UPDATE "pg_catalog"."pg_settings" SET setting = 'x' WHERE name = 'role'`,
		"UPDATE /* catalog */ pg_catalog /* dot */ . pg_settings SET setting = 'x'",
		"WITH s AS (UPDATE pg_settings SET setting = 'x' WHERE name = 'role' RETURNING 1) SELECT * FROM s",
		"SELECT 'safe'; UPDATE pg_settings SET setting = 'x'",
		`SELECT $$x$$, E'can\'t'; UPDATE pg_settings SET setting = 'x'`,
	} {
		for name, guard := range map[string]func(string) error{"default": GuardNoSessionEscape, "scoped": scoped, "strict": strict, "read-only": GuardReadOnlyTransaction} {
			require.ErrorIs(t, guard(q), ErrForbiddenStatement, "%s: %s", name, q)
		}
	}
	for _, q := range []string{
		"SELECT name, setting FROM pg_settings WHERE name = 'TimeZone'",
		"SELECT 'UPDATE pg_settings SET setting = 1'",
		"SELECT 1 /* UPDATE pg_settings */ -- UPDATE pg_settings",
		`SELECT $q$UPDATE pg_settings SET setting = 1$q$`,
		`UPDATE "PG_SETTINGS" SET setting = 'x'`,
		"UPDATE items SET pg_settings = $1",
		"INSERT INTO items (id) VALUES (1) ON CONFLICT (id) DO UPDATE SET pg_settings = EXCLUDED.pg_settings",
	} {
		require.NoError(t, GuardNoSessionEscape(q), q)
		require.NoError(t, scoped(q), q)
	}
	open, err := NewSessionGuard(SessionGuardConfig{})
	require.NoError(t, err)
	require.NoError(t, open("UPDATE pg_settings SET setting = 'x'"), "a zero config permits all statements")
}

func TestSessionGuardVariableReads(t *testing.T) {
	guard, err := NewSessionGuard(SessionGuardConfig{VariablePrefixes: []string{"Scope."}, ForbidSetConfig: true, ForbidVariableReads: true})
	require.NoError(t, err)
	for _, q := range []string{
		"SELECT current_setting('scope.id')",
		"SELECT 1 WHERE current_setting('SCOPE.id', true) IS NULL",
		"SELECT pg_catalog.current_setting('scope.id', true)",
		`SELECT "current_setting"('scope.id')`,
		"SELECT current_setting($1)",
		"SELECT current_setting('scope.' || 'id')",
		`SELECT current_setting(E'scope\.id')`,
		"SELECT setting FROM pg_settings WHERE name = 'TimeZone'",
		"SELECT setting FROM pg_catalog.pg_settings",
		"SELECT 1 AS pg_settings FROM pg_settings",
		"SELECT id AS pg_settings FROM items, pg_catalog.pg_settings",
		"SELECT 1 AS pg_settings; TABLE pg_settings",
		"SELECT (SELECT count(*) FROM pg_settings) AS pg_settings",
		"SELECT 1 AS pg_settings FROM items JOIN pg_catalog.pg_settings AS s ON true",
		"SELECT 1 AS pg_settings, current_setting('scope.id')",
		"SHOW scope.id", `SHOW "scope"."id"`, "SHOW ALL",
		"SELECT 'safe'; SHOW scope.id",
	} {
		require.ErrorIs(t, guard(q), ErrForbiddenStatement, q)
	}
	for _, q := range []string{
		"SELECT current_setting('TimeZone')",
		"SELECT current_setting('statement_timeout'), current_setting('search_path', true)",
		"SHOW TimeZone",
		"SELECT 'current_setting(''scope.id'')'",
		`SELECT $$SHOW scope.id; SELECT * FROM pg_settings$$`,
		"SELECT 1 /* current_setting('scope.id') */",
		"SELECT current_settings, pg_settings_id FROM items",
		"SELECT 1 AS pg_settings",
		`SELECT id aS /* output name */ "pg_settings" FROM items`,
		"SELECT id pg_settings FROM items",
		"SELECT 1 AS pg_settings ORDER BY pg_settings",
		"SELECT id, 1 AS pg_settings FROM items GROUP BY id, pg_settings ORDER BY id, pg_settings",
		"SELECT id FROM items UNION ALL SELECT id pg_settings FROM entries",
		"SELECT (SELECT 1 AS pg_settings) AS pg_settings",
		"UPDATE items SET id = $1 RETURNING id AS pg_settings",
	} {
		require.NoError(t, guard(q), q)
	}
	_, err = NewSessionGuard(SessionGuardConfig{ForbidVariableReads: true})
	require.Error(t, err, "variable reads need a protected namespace")
}
