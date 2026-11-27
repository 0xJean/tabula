package tabula

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func signedPolicyCall(query string, effects statementEffects) (SQLAuthority, *int64, AuthorizedStatement) {
	authority := SQLAuthority{issuer: &statementIssuer{}}
	owner := new(int64)
	shape := &compiledShape{authority: authority.issuer, sql: query,
		shape: StatementShape{Effects: EffectWrite}, effects: effects}
	return authority, owner, AuthorizedStatement{method: CallReturningEach, shape: shape, owner: owner}
}

func TestSignedWriteCTEPolicy(t *testing.T) {
	effects := statementEffects{writeCTE: true, writes: []statementWrite{{command: "INSERT"}}}
	policy := StatementPolicy{Read: []string{"select"}, Write: []string{"with", "insert"}, RequireReturning: true, AllowAuthorizedWriteCTE: true}
	guard, err := policy.Compile(false)
	require.NoError(t, err)
	for _, query := range []string{
		"WITH added AS (INSERT INTO items VALUES ($1) RETURNING id) SELECT id FROM added",
		`WITH "added" AS MATERIALIZED (INSERT INTO items(note) VALUES ($1) RETURNING id) SELECT id FROM "added"`,
		"WITH added AS (INSERT INTO items(note) VALUES ('semi;colon') RETURNING id) SELECT id FROM added; -- done",
		"WITH added AS (INSERT INTO items(note) VALUES ($value$; DELETE FROM items$value$) RETURNING id) SELECT id FROM added",
	} {
		authority, owner, call := signedPolicyCall(query, effects)
		require.NoError(t, guard.CheckAuthorized(authority, owner, CallReturningEach, call), query)
		require.ErrorIs(t, guard.Check("Returning", query), ErrForbiddenStatement)
		require.ErrorIs(t, guard.CheckAuthorized(authority, new(int64), CallReturningEach, call), ErrStatementAuthority)
		require.ErrorIs(t, guard.CheckAuthorized(SQLAuthority{}, owner, CallReturningEach, call), ErrStatementAuthority)
		require.ErrorIs(t, guard.CheckAuthorized(authority, owner, CallEach, call), ErrStatementAuthority)
		view, err := authority.Open(owner, CallReturningEach, call)
		require.NoError(t, err)
		view.Query = "COMMIT"
		view.Effects = EffectRead
		require.NoError(t, guard.CheckAuthorized(authority, owner, CallReturningEach, call))
		unsigned := *call.shape
		unsigned.effects = statementEffects{}
		call.shape = &unsigned
		require.ErrorIs(t, guard.CheckAuthorized(authority, owner, CallReturningEach, call), ErrForbiddenStatement)
	}
	for _, query := range []string{
		"WITH added AS (INSERT INTO items VALUES ($1) RETURNING id) SELECT id FROM added; COMMIT",
		"WITH added AS (DELETE FROM items RETURNING id) SELECT id FROM added",
		"WITH added AS (INSERT INTO items VALUES ($1) RETURNING id) SELECT id INTO entries FROM added",
		"WITH added AS (INSERT INTO items VALUES ('unfinished) RETURNING id) SELECT id FROM added",
		"WITH added AS (INSERT INTO items VALUES ($1) RETURNING id) SELECT id FROM added /*",
	} {
		authority, owner, call := signedPolicyCall(query, effects)
		require.ErrorIs(t, guard.CheckAuthorized(authority, owner, CallReturningEach, call), ErrForbiddenStatement, query)
	}
	policy.AllowAuthorizedWriteCTE = false
	disabled, err := policy.Compile(false)
	require.NoError(t, err)
	authority, owner, call := signedPolicyCall("WITH added AS (INSERT INTO items VALUES ($1) RETURNING id) SELECT id FROM added", effects)
	require.ErrorIs(t, disabled.CheckAuthorized(authority, owner, CallReturningEach, call), ErrForbiddenStatement)
	policy.AllowAuthorizedWriteCTE, policy.Write = true, []string{"insert"}
	noWith, err := policy.Compile(false)
	require.NoError(t, err)
	require.ErrorIs(t, noWith.CheckAuthorized(authority, owner, CallReturningEach, call), ErrForbiddenStatement)
	call.shape.effects.writes = append(call.shape.effects.writes, statementWrite{command: "UPDATE"})
	require.ErrorIs(t, guard.CheckAuthorized(authority, owner, CallReturningEach, call), ErrForbiddenStatement)
}

func TestSignedWriteRoutinePolicy(t *testing.T) {
	policy := StatementPolicy{AllowAuthorizedWriteRoutine: true}
	guard, err := policy.Compile(false)
	require.NoError(t, err)
	require.True(t, guard.Enabled())
	authority, owner, call := signedPolicyCall("SELECT public.append_note($1) AS id", statementEffects{writeRoutine: true})
	call.shape.shape.Routines = []RoutineRequirement{{Signature: "public.append_note(text)"}}
	call.shape.shape.Privileges = []DefinedPrivilege{{Function: "public.append_note(text)", Privilege: "EXECUTE"}}
	require.NoError(t, guard.CheckAuthorized(authority, owner, CallReturningEach, call))
	for _, method := range []string{"Scan", "Each", "Returning", "Exec"} {
		require.ErrorIs(t, guard.Check(method, call.shape.sql), ErrForbiddenStatement)
	}
	call.shape.shape.Privileges[0].Function = "public.other(text)"
	require.ErrorIs(t, guard.CheckAuthorized(authority, owner, CallReturningEach, call), ErrForbiddenStatement)
	call.shape.shape.Privileges[0].Function = "public.append_note(text)"
	call.shape.shape.Routines[0].ReadOnly = true
	require.ErrorIs(t, guard.CheckAuthorized(authority, owner, CallReturningEach, call), ErrForbiddenStatement)
	call.shape.shape.Routines[0].ReadOnly = false
	call.shape.sql += "; COMMIT"
	require.ErrorIs(t, guard.CheckAuthorized(authority, owner, CallReturningEach, call), ErrForbiddenStatement)
}

func TestSignedUpsertChecksEveryCommand(t *testing.T) {
	authority, owner, call := signedPolicyCall("INSERT INTO items VALUES ($1) ON CONFLICT(id) DO UPDATE SET note=$2 RETURNING id", statementEffects{writes: []statementWrite{{command: "INSERT"}, {command: "UPDATE"}}})
	insertOnly, err := (StatementPolicy{Write: []string{"insert"}}).Compile(false)
	require.NoError(t, err)
	require.ErrorIs(t, insertOnly.CheckAuthorized(authority, owner, CallReturningEach, call), ErrForbiddenStatement)
	both, err := (StatementPolicy{Write: []string{"insert", "update"}, RequireReturning: true}).Compile(false)
	require.NoError(t, err)
	require.NoError(t, both.CheckAuthorized(authority, owner, CallReturningEach, call))
	unrestricted, err := (StatementPolicy{}).Compile(false)
	require.NoError(t, err)
	require.NoError(t, unrestricted.CheckAuthorized(authority, owner, CallReturningEach, call))
}
