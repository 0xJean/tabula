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

type rowCallback func(context.Context, tabula.Session[itemScope]) error

func runRowPlan(t *testing.T, db *sql.DB, pool *pgxpool.Pool, driver string, auth tabula.SQLAuthority, role string, scope itemScope, log tabula.LogConfig, body rowCallback) error {
	t.Helper()
	vars := func(s itemScope) map[string]string {
		return map[string]string{"demo.scope": strconv.FormatInt(int64(s), 10)}
	}
	if driver == "stdsql" {
		r, err := stdsql.NewRunner(db, stdsql.ScopePolicy[itemScope]{ManagedVars: []string{"demo.scope"}, ScopeVars: vars},
			stdsql.RunnerOptions{Role: role, SQLAuthority: auth, Log: log, RequireCallbackContext: true})
		require.NoError(t, err)
		return r.Within(t.Context(), scope, func(ctx context.Context, s *stdsql.Session[itemScope]) error {
			return body(ctx, s)
		})
	}
	r, err := pgxdriver.NewRunner(pool, pgxdriver.ScopePolicy[itemScope]{ManagedVars: []string{"demo.scope"}, ScopeVars: vars},
		pgxdriver.RunnerOptions{Role: role, SQLAuthority: auth, Log: log, RequireCallbackContext: true})
	require.NoError(t, err)
	return r.Within(t.Context(), scope, func(ctx context.Context, s *pgxdriver.Session[itemScope]) error {
		return body(ctx, s)
	})
}
