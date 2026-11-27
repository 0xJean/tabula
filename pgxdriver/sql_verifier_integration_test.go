//go:build integration

package pgxdriver_test

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
)

func TestSQLVerifierProtocolLifecycle(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn, "TABULA_TEST_DSN is required for integration tests")
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	ns := "tabula_describe_" + strings.ToLower(rand.Text())
	_, err = pool.Exec(ctx, "CREATE SCHEMA "+ns)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA "+ns+" CASCADE")
		require.NoError(t, err)
	})
	_, err = pool.Exec(ctx, "CREATE DOMAIN "+ns+".kind AS text; CREATE SEQUENCE "+ns+".entries; "+
		"CREATE TABLE "+ns+".items(id bigint DEFAULT nextval('"+ns+".entries'), note "+ns+".kind); "+
		"CREATE TABLE "+ns+".notes(value text); "+
		"CREATE FUNCTION "+ns+".write_note(v text) RETURNS integer LANGUAGE plpgsql VOLATILE AS $$ "+
		"BEGIN INSERT INTO "+ns+".notes(value) VALUES(v); PERFORM nextval('"+ns+".entries'); RETURN 1; END $$")
	require.NoError(t, err)
	verifier := pgxdriver.SQLVerifier(pool)
	tx, err := verifier.BeginVerification(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	var mode string
	require.NoError(t, tx.Scan(ctx, "SHOW transaction_read_only", nil, &mode))
	require.Equal(t, "on", mode)

	var domainOID, textOID, int8OID uint32
	require.NoError(t, tx.Scan(ctx, "SELECT pg_catalog.to_regtype($1)::oid, 'pg_catalog.text'::regtype::oid, 'pg_catalog.int8'::regtype::oid", []any{ns + ".kind"}, &domainOID, &textOID, &int8OID))
	result, err := tx.Describe(ctx, "tabula_insert_description", "INSERT INTO "+ns+".items(note) VALUES(CAST($1 AS "+ns+".kind)) RETURNING id, note")
	require.NoError(t, err)
	require.Equal(t, []uint32{domainOID}, result.Parameters)
	require.Len(t, result.Fields, 2)
	require.Equal(t, "id", result.Fields[0].Name)
	require.Equal(t, int8OID, result.Fields[0].TypeOID)
	require.Equal(t, textOID, result.Fields[1].TypeOID)
	require.NotZero(t, result.Fields[1].TableOID)
	require.Equal(t, int16(2), result.Fields[1].Attribute)

	for i, query := range []string{
		"WITH added AS (INSERT INTO " + ns + ".items(note) VALUES($1) RETURNING id) SELECT id FROM added",
		"SELECT " + ns + ".write_note($1) AS value",
	} {
		name := []string{"tabula_cte_description", "tabula_function_description"}[i]
		result, err := tx.Describe(ctx, name, query)
		require.NoError(t, err)
		require.Len(t, result.Fields, 1)
		var count int
		require.NoError(t, tx.Scan(ctx, "SELECT count(*) FROM pg_catalog.pg_prepared_statements WHERE name = $1", []any{name}, &count))
		require.Zero(t, count)
	}
	var rows int
	var called bool
	require.NoError(t, tx.Scan(ctx, "SELECT count(*) FROM "+ns+".items", nil, &rows))
	require.Zero(t, rows)
	require.NoError(t, tx.Scan(ctx, "SELECT count(*) FROM "+ns+".notes", nil, &rows))
	require.Zero(t, rows)
	require.NoError(t, tx.Scan(ctx, "SELECT is_called FROM "+ns+".entries", nil, &called))
	require.False(t, called)

	require.NoError(t, tx.Each(ctx, "SELECT 1", nil, func(scan func(...any) error) error {
		var value int
		require.ErrorIs(t, tx.Scan(ctx, "SELECT 2", nil, &value), tabula.ErrSessionBusy)
		_, err := tx.Describe(ctx, "tabula_overlap", "SELECT 2")
		require.ErrorIs(t, err, tabula.ErrSessionBusy)
		return scan(&value)
	}))

	require.NoError(t, tx.Exec(ctx, "PREPARE tabula_existing AS SELECT 7"))
	require.NoError(t, tx.Exec(ctx, "SAVEPOINT tabula_collision"))
	_, err = tx.Describe(ctx, "tabula_existing", "SELECT 8")
	require.Equal(t, "42P05", tabula.SQLState(err))
	require.NoError(t, tx.Exec(ctx, "ROLLBACK TO SAVEPOINT tabula_collision"))
	var value int
	require.NoError(t, tx.Scan(ctx, "EXECUTE tabula_existing", nil, &value))
	require.Equal(t, 7, value)
	require.NoError(t, tx.Exec(ctx, "DEALLOCATE tabula_existing"))
	require.NoError(t, tx.Exec(ctx, "RELEASE SAVEPOINT tabula_collision"))
	require.NoError(t, tx.Rollback(ctx))
	_, err = tx.Describe(ctx, "tabula_closed", "SELECT 1")
	require.ErrorIs(t, err, pgx.ErrTxClosed)
	require.ErrorIs(t, tx.Exec(ctx, "SELECT 1"), pgx.ErrTxClosed)
	require.ErrorIs(t, tx.Scan(ctx, "SELECT 1", nil, &value), pgx.ErrTxClosed)
	require.ErrorIs(t, tx.Rollback(ctx), pgx.ErrTxClosed)
}
