//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"strconv"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/stdsql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// Verify the issued call before delivering it to a real role-bound runner.
type rowTestSession struct {
	tabula.Session[itemScope]
	authority tabula.SQLAuthority
}

func (s *rowTestSession) ExecAuthorized(ctx context.Context, call tabula.AuthorizedStatement) (int64, error) {
	q, err := s.authority.Open(s, tabula.CallExec, call)
	if err != nil {
		return 0, err
	}
	return s.Exec(ctx, q.Query, q.Args...)
}
func (s *rowTestSession) ScanAuthorized(ctx context.Context, call tabula.AuthorizedStatement, dest ...any) error {
	q, err := s.authority.Open(s, tabula.CallScan, call)
	if err != nil {
		return err
	}
	return s.Scan(ctx, q.Query, q.Args, dest...)
}
func (s *rowTestSession) EachAuthorized(ctx context.Context, call tabula.AuthorizedStatement, visit func(func(...any) error) error) error {
	q, err := s.authority.Open(s, tabula.CallEach, call)
	if err != nil {
		return err
	}
	return s.Each(ctx, q.Query, q.Args, visit)
}

type rowCallback func(context.Context, tabula.Session[itemScope]) error

func runRowPlan(t *testing.T, db *sql.DB, pool *pgxpool.Pool, driver string, auth tabula.SQLAuthority, role string, scope itemScope, body rowCallback) error {
	t.Helper()
	vars := func(s itemScope) map[string]string {
		return map[string]string{"demo.scope": strconv.FormatInt(int64(s), 10)}
	}
	if driver == "stdsql" {
		r, err := stdsql.NewRunner(db, stdsql.ScopePolicy[itemScope]{ManagedVars: []string{"demo.scope"}, ScopeVars: vars},
			stdsql.RunnerOptions{Role: role, RequireCallbackContext: true})
		require.NoError(t, err)
		return r.Within(t.Context(), scope, func(ctx context.Context, s *stdsql.Session[itemScope]) error {
			return body(ctx, &rowTestSession{s, auth})
		})
	}
	r, err := pgxdriver.NewRunner(pool, pgxdriver.ScopePolicy[itemScope]{ManagedVars: []string{"demo.scope"}, ScopeVars: vars},
		pgxdriver.RunnerOptions{Role: role, RequireCallbackContext: true})
	require.NoError(t, err)
	return r.Within(t.Context(), scope, func(ctx context.Context, s *pgxdriver.Session[itemScope]) error {
		return body(ctx, &rowTestSession{s, auth})
	})
}
