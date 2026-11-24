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
		require.ErrorIs(t, GuardReadOnlyTransaction(query), ErrReadOnly, query)
	}
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
	} {
		require.ErrorIs(t, guard(q), ErrForbiddenStatement, q)
	}
	for _, q := range []string{"SELECT 'RESET ALL'", "SET LOCAL unrelated.id = 'a'", "RESET timezone", "UPDATE items SET role=$1", "SELECT set_config('unrelated.id','a',true)", `SELECT $$set_config('scope.id','a',true)$$`} {
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
