//go:build integration

package stdsql_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
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
		Tenant tenant `db:"tenant_id,tenant"`
		Body   string `db:"body"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[tenant]{
		TenantID: func(s tenant) any { return string(s) },
	})
	notes := tabula.Define(reg, tabula.Table[note, int64, tenant]{Name: schema + ".notes", Order: "id ASC"})

	runner := stdsql.NewRunner(db, stdsql.ScopePolicy[tenant]{
		SetupTx: func(ctx context.Context, tx *sql.Tx, scope tenant) error {
			return stdsql.SetLocalConfig(ctx, tx, "app.current_tenant", string(scope))
		},
		GuardQuery: func(q string) error {
			if strings.Contains(strings.ToUpper(q), "SET ROLE") {
				return errors.New("forbidden statement")
			}
			return nil
		},
	})

	// Scope SetupTx + Savepoint rollback keeps outer transaction intact.
	require.NoError(t, runner.Within(ctx, "t-1", func(ctx context.Context, s *stdsql.Session[tenant]) error {
		var current string
		require.NoError(t, s.Scan(ctx, "t-1", "SELECT current_setting('app.current_tenant')", nil, &current))
		require.Equal(t, "t-1", current)

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

		// GuardQuery blocks forbidden SQL.
		_, err := s.Exec(ctx, "t-1", "SET ROLE postgres")
		require.ErrorContains(t, err, "forbidden statement")

		all, err := r.Find(ctx, "t-1", nil, tabula.Listing{})
		require.NoError(t, err)
		require.Len(t, all, 1)
		require.Equal(t, "kept", all[0].Body)
		return nil
	}))

	// WithinReadOnly rejects writes in PostgreSQL.
	err = runner.WithinReadOnly(ctx, "t-1", func(ctx context.Context, s *stdsql.Session[tenant]) error {
		row := note{Tenant: "t-1", Body: "nope"}
		return notes.Bind(s).Insert(ctx, &row)
	})
	require.Equal(t, "25006", tabula.SQLState(err))

	// WithinIsolation works with Serializable + Retry helper.
	require.NoError(t, stdsql.Retry(ctx, stdsql.RetryConfig{MaxAttempts: 3}, func(ctx context.Context) error {
		return runner.WithinIsolation(ctx, "t-1", sql.LevelSerializable, func(ctx context.Context, s *stdsql.Session[tenant]) error {
			rows, err := notes.Bind(s).Find(ctx, "t-1", nil, tabula.Listing{})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			return nil
		})
	}))
}
