package tabula_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
)

func TestGuardSingleStatement(t *testing.T) {
	for _, sql := range []string{
		"SELECT 1", "SELECT 1; -- end", "(VALUES (1))", "TABLE items", "UPDATE items SET label = $1",
		`SELECT 'one;''two', "label;part", $tag$one;two$tag$, E'can\'t; split'`,
		"SELECT 1 /* outer /* ; */ comment */; /* end */", "SELECT 'héllo'",
	} {
		require.NoError(t, tabula.GuardSingleStatement(sql), sql)
	}
	for _, sql := range []string{
		"", "-- empty", ";", "CREATE TABLE items (id int)", "SELECT 1; SELECT 2", "SELECT 1;;",
		"SELECT 'unfinished", `SELECT "unfinished`, "SELECT $tag$unfinished", "SELECT 1 /* unfinished",
		"SELECT (1", "SELECT 1)", "SELECT 1\x00", "SELECT '\xff'", `SELECT 'one\'two'`,
	} {
		require.ErrorIs(t, tabula.GuardSingleStatement(sql), tabula.ErrForbiddenStatement, sql)
	}
}

func TestReadOnlyStatementBoundaries(t *testing.T) {
	require.NoError(t, tabula.GuardReadOnly(`SELECT E'can\'t; split';`))
	require.ErrorIs(t, tabula.GuardReadOnly(`SELECT 'can\'t; split'`), tabula.ErrReadOnly)
}

func TestStatementPolicy(t *testing.T) {
	policy := tabula.StatementPolicy{
		Read: []string{"SELECT", "with"}, Write: []string{"insert", "UPDATE", "with"}, RequireReturning: true,
	}
	guard, err := policy.Compile(false)
	require.NoError(t, err)
	policy.Read[0] = "table"
	for _, tc := range []struct{ method, sql string }{
		{"Scan", "SELECT 1;"}, {"Each", "(SELECT 1)"}, {"Each", "SELECT id FROM items FOR UPDATE"},
		{"Scan", `WITH RECURSIVE a(id) AS NOT MATERIALIZED (VALUES (1)), "b" AS (SELECT * FROM a) SELECT * FROM "b"`},
		{"Scan", "WITH a AS (WITH b AS (SELECT 1) SELECT * FROM b) SELECT * FROM a"},
		{"Exec", "INSERT INTO items (id) VALUES ($1)"}, {"Returning", "UPDATE items SET id = $1 RETURNING id"},
		{"Returning", "WITH x AS (SELECT 1) INSERT INTO items SELECT * FROM x RETURNING id"},
		{"Exec", "WITH x AS (UPDATE items SET id = $1 RETURNING id) INSERT INTO items SELECT * FROM x"},
	} {
		require.NoError(t, guard.Check(tc.method, tc.sql), tc.sql)
	}
	for _, tc := range []struct{ method, sql string }{
		{"Scan", "SELECT 1; SELECT 2"}, {"Each", "UPDATE items SET id = $1 RETURNING id"},
		{"Scan", "SELECT id INTO snapshot FROM items"}, {"Scan", "TABLE items"},
		{"Each", "WITH x AS (UPDATE items SET id = $1 RETURNING id) SELECT id FROM x"},
		{"Scan", "WITH x AS (SELECT 1) UPDATE items SET id = $1"},
		{"Scan", "WITH x AS (WITH y AS (DELETE FROM items RETURNING id) SELECT id FROM y) SELECT id FROM x"},
		{"Exec", "SELECT 1"}, {"Exec", "DELETE FROM items"},
		{"Exec", "WITH x AS (DELETE FROM items RETURNING id) INSERT INTO items SELECT * FROM x"},
		{"Returning", "SELECT id FROM items"}, {"Returning", "UPDATE items SET label = 'returning' /* RETURNING */"},
		{"Returning", `UPDATE items SET label = "returning"`},
		{"Returning", "UPDATE items SET id = (SELECT id AS returning FROM items LIMIT 1)"},
		{"Returning", "WITH x AS (UPDATE items SET id = $1 RETURNING id) INSERT INTO items SELECT * FROM x"},
		{"other", "SELECT 1"},
	} {
		require.ErrorIs(t, guard.Check(tc.method, tc.sql), tabula.ErrForbiddenStatement, tc.sql)
	}
}

func TestStatementPolicyConfiguration(t *testing.T) {
	for _, policy := range []tabula.StatementPolicy{
		{Read: []string{"update"}}, {Write: []string{"select"}}, {Read: []string{"with"}},
		{Write: []string{"execute"}}, {RequireReturning: true},
	} {
		_, err := policy.Compile(false)
		require.Error(t, err)
	}
	defaults, err := (tabula.StatementPolicy{}).Compile(false)
	require.NoError(t, err)
	require.NoError(t, defaults.Check("Exec", "SELECT 1; SELECT 2"))
	empty, err := (tabula.StatementPolicy{Read: []string{}, Write: []string{}}).Compile(false)
	require.NoError(t, err)
	require.False(t, empty.Enabled())
	single, err := (tabula.StatementPolicy{}).Compile(true)
	require.NoError(t, err)
	require.NoError(t, single.Check("Exec", "SELECT 1"))
	require.ErrorIs(t, single.Check("Exec", "SELECT 1; SELECT 2"), tabula.ErrForbiddenStatement)
	reads, err := (tabula.StatementPolicy{Read: []string{"select"}}).Compile(false)
	require.NoError(t, err)
	require.ErrorIs(t, reads.Check("Exec", "INSERT INTO items VALUES (1)"), tabula.ErrForbiddenStatement)
	require.ErrorIs(t, reads.Check("Scan", "WITH x AS (SELECT 1) SELECT * FROM x"), tabula.ErrForbiddenStatement)
	writes, err := (tabula.StatementPolicy{Write: []string{"insert"}}).Compile(false)
	require.NoError(t, err)
	require.NoError(t, writes.Check("Returning", "INSERT INTO items VALUES (1)"))
}
