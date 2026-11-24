//go:build integration

package stdsql_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/stdsql"
)

func TestRunnerScopeSavepointAndRetry(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn, "TABULA_TEST_DSN is required for integration tests")
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()

	schema := fmt.Sprintf("tabula_stdsql_it_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE TABLE `+schema+`.notes (
		id bigserial PRIMARY KEY,
		tenant_id text NOT NULL,
		body text NOT NULL
	)`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })

	type tenant string
	type note struct {
		ID     int64  `db:"id,key,serial"`
		Tenant tenant `db:"tenant_id,scope"`
		Body   string `db:"body"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[tenant]{
		ScopeID: func(s tenant) any { return string(s) },
	})
	notes := tabula.Define(reg, tabula.TableSpec[note, int64]{Name: schema + ".notes", Order: "id ASC"})

	guard, err := tabula.NewSessionGuard(tabula.SessionGuardConfig{
		VariablePrefixes: []string{"session.", "local."},
		ForbidRole:       true,
	})
	require.NoError(t, err)
	runner, err := stdsql.NewRunner(db, stdsql.ScopePolicy[tenant]{
		ManagedVars: []string{"session.scope", "local.scope"},
		ScopeVars: func(scope tenant) map[string]string {
			return map[string]string{
				"session.scope": string(scope),
				"local.scope":   string(scope),
			}
		},
		GuardQuery: guard,
	}, stdsql.RunnerOptions{})
	require.NoError(t, err)

	// Scope vars + Savepoint rollback keeps outer transaction intact.
	require.NoError(t, runner.Within(ctx, "t-1", func(ctx context.Context, s *stdsql.Session[tenant]) error {
		for _, q := range []string{
			"SET ROLE postgres",
			"DO $$BEGIN NULL; END;$$",
			"SET session.scope = 'other'",
			"SET local.scope = 'other'",
		} {
			_, err := s.Exec(ctx, q)
			require.ErrorIs(t, err, tabula.ErrForbiddenStatement, q)
		}
		var sessionScope, localScope string
		require.NoError(t, s.Scan(ctx, "SELECT current_setting('session.scope'), current_setting('local.scope')", nil, &sessionScope, &localScope))
		require.Equal(t, "t-1", sessionScope)
		require.Equal(t, "t-1", localScope)

		r := notes.Bind(s)
		first := note{Tenant: "t-1", Body: "kept"}
		require.NoError(t, r.Insert(ctx, &first))

		// Roll back a nested savepoint.
		spErr := s.Savepoint(ctx, "inner", func(ctx context.Context) error {
			second := note{Tenant: "t-1", Body: "discarded"}
			require.NoError(t, r.Insert(ctx, &second))
			return errors.New("abort inner")
		})
		require.ErrorContains(t, spErr, "abort inner")

		all, err := r.Find(ctx, nil, tabula.Listing{})
		require.NoError(t, err)
		require.Len(t, all, 1)
		require.Equal(t, "kept", all[0].Body)
		return nil
	}))

	// WithinReadOnly rejects derived writes immediately and maps PG 25006 to ErrReadOnly.
	err = runner.WithinReadOnly(ctx, "t-1", func(ctx context.Context, s *stdsql.Session[tenant]) error {
		require.True(t, s.ReadOnly())
		row := note{Tenant: "t-1", Body: "nope"}
		require.ErrorIs(t, notes.Bind(s).Insert(ctx, &row), tabula.ErrReadOnly)
		_, rawErr := s.Exec(ctx, "INSERT INTO "+schema+".notes (tenant_id, body) VALUES ($1, $2)", "t-1", "nope")
		return rawErr
	})
	require.ErrorIs(t, err, tabula.ErrReadOnly)
	require.Equal(t, "25006", tabula.SQLState(err))

	// WithinIsolation works with Serializable + WithinRetryWith helper.
	require.NoError(t, runner.WithinRetryWith(ctx, "t-1", tabula.RetryConfig{MaxAttempts: 3}, stdsql.RunOptions[tenant]{
		TxOptions: &sql.TxOptions{Isolation: sql.LevelSerializable},
	}, func(ctx context.Context, s *stdsql.Session[tenant]) error {
		rows, err := notes.Bind(s).Find(ctx, nil, tabula.Listing{})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		return nil
	}))
}
