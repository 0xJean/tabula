//go:build integration

package plan_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"os"
	"strings"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/stdsql"
	"github.com/stretchr/testify/require"
)

func TestSignedWriteCTEMethods(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ns := "tabula_policy_" + strings.ToLower(rand.Text())
	_, err = db.ExecContext(t.Context(), "CREATE SCHEMA "+ns)
	require.NoError(t, err)
	t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + ns + " CASCADE"); require.NoError(t, err) })
	_, err = db.ExecContext(t.Context(), "CREATE TABLE "+ns+".items(id bigint PRIMARY KEY)")
	require.NoError(t, err)
	authority, bind, err := plan.SignedPolicyFixture(ns + ".items")
	require.NoError(t, err)
	policy := q.StatementPolicy{Read: []string{"select"}, Write: []string{"with", "insert"}, RequireReturning: true}
	runner := func() *stdsql.Runner[struct{}] {
		t.Helper()
		r, err := stdsql.NewRunner(db, stdsql.NoScopePolicy(), stdsql.RunnerOptions{SQLAuthority: authority, Statements: policy})
		require.NoError(t, err)
		return r
	}
	execute := func(ctx context.Context, s *stdsql.Session[struct{}]) error {
		call, err := bind(s, 7)
		if err != nil {
			return err
		}
		return s.EachAuthorized(ctx, call, func(scan func(...any) error) error { var id int64; return scan(&id) })
	}
	require.ErrorIs(t, runner().Within(t.Context(), struct{}{}, execute), q.ErrForbiddenStatement)
	policy.AllowAuthorizedWriteCTE = true
	require.NoError(t, runner().Within(t.Context(), struct{}{}, execute))
	require.ErrorIs(t, runner().WithinReadOnly(t.Context(), struct{}{}, execute), q.ErrReadOnly)
	var n int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+ns+".items").Scan(&n))
	require.Equal(t, 1, n)
}
