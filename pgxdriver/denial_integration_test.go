//go:build integration

package pgxdriver_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestNativeDenialClasses(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	ctx := t.Context()
	c, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = c.Close(context.Background()) }()
	tx, err := c.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	ns := fmt.Sprintf("tabula_pgxd_%d", time.Now().UnixNano())
	role := ns + "_writer"
	table := ns + ".notes"
	for _, q := range []string{"CREATE SCHEMA " + ns, "CREATE ROLE " + role, "CREATE TABLE " + table + "(id int)", "GRANT USAGE ON SCHEMA " + ns + " TO " + role, "GRANT INSERT ON " + table + " TO " + role, "ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY", "SET LOCAL ROLE " + role} {
		_, err = tx.Exec(ctx, q)
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		q    string
		want tabula.ErrorClass
	}{{"INSERT INTO " + table + " VALUES (1)", tabula.ClassRLSDenied}, {"SELECT id FROM " + table, tabula.ClassInsufficientPrivilege}} {
		_, err = tx.Exec(ctx, "SAVEPOINT probe")
		require.NoError(t, err)
		_, err = tx.Exec(ctx, tc.q)
		require.Equal(t, "42501", tabula.SQLState(err))
		class, _ := tabula.ClassifyError(ctx, err)
		require.Equal(t, tc.want, class)
		_, err = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT probe")
		require.NoError(t, err)
	}
}
