//go:build integration

package postgresfixture_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0xJean/tabula/postgresfixture"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestPrunePreservesUnrecognizedDatabases(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	f := postgresfixture.Fixture{Admin: admin, Backend: &installer{dsn: dsn}}
	_, err = f.Prune(ctx, 0)
	require.Error(t, err)
	_, err = f.Prune(ctx, time.Hour, postgresfixture.Installation{})
	require.Error(t, err)

	for _, tc := range []struct{ label, mark string }{
		{"similar stage", ""},
		{"missing mark", ""},
		{"invalid calendar date", "tabula fixture template; last used 2000-02-31T00:00:00Z"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			plan := postgresfixture.Installation{Revision: t.TempDir()}
			name, err := plan.TemplateName()
			require.NoError(t, err)
			if tc.label == "similar stage" {
				name = "tabula_stg_other_" + strings.TrimPrefix(name, "tabula_tpl_")[:12]
			}
			quoted := pq.QuoteIdentifier(name)
			_, err = admin.ExecContext(t.Context(), "CREATE DATABASE "+quoted+" TEMPLATE template0")
			require.NoError(t, err)
			t.Cleanup(func() { _, err := admin.Exec("DROP DATABASE IF EXISTS " + quoted); require.NoError(t, err) })
			if tc.mark != "" {
				_, err = admin.ExecContext(t.Context(), "COMMENT ON DATABASE "+quoted+" IS "+pq.QuoteLiteral(tc.mark))
				require.NoError(t, err)
			}
			dropped, err := f.Prune(t.Context(), time.Hour)
			require.NoError(t, err)
			require.NotContains(t, dropped, name)
			var present bool
			require.NoError(t, admin.QueryRowContext(t.Context(), "SELECT EXISTS (SELECT FROM pg_database WHERE datname=$1)", name).Scan(&present))
			require.True(t, present)
		})
	}
}

type inspectCopy struct {
	postgresfixture.Backend
	template string
	inspect  func(context.Context, *sql.Conn) error
}

func (b inspectCopy) Clone(ctx context.Context, conn *sql.Conn, from, to string) error {
	if from == b.template {
		if err := b.inspect(ctx, conn); err != nil {
			return err
		}
	}
	return b.Backend.Clone(ctx, conn, from, to)
}

func TestPruneSkipsTemplateDuringParallelCopies(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	f := postgresfixture.Fixture{Admin: admin, Backend: &installer{dsn: dsn}}
	plan := postgresfixture.Installation{Revision: t.TempDir(), Base: []byte("CREATE TABLE notes(id int)")}
	name, err := plan.TemplateName()
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec("DROP DATABASE IF EXISTS " + pq.QuoteIdentifier(name))
		require.NoError(t, err)
	})
	probe := postgresfixture.Fixture{Admin: admin, Backend: inspectCopy{f.Backend, name, func(ctx context.Context, conn *sql.Conn) error {
		// Make the template eligible by age while the first physical copy uses it.
		_, err := conn.ExecContext(ctx, "COMMENT ON DATABASE "+pq.QuoteIdentifier(name)+" IS 'tabula fixture template; last used 2000-01-01T00:00:00Z'")
		require.NoError(t, err)
		dropped, err := f.Prune(ctx, time.Hour)
		require.NoError(t, err)
		require.NotContains(t, dropped, name)
		// A second worker can copy the ready template before the first returns.
		peer, err := f.Clone(ctx, plan)
		if err != nil {
			return err
		}
		return peer.Drop(ctx)
	}}}
	clone, err := probe.Clone(ctx, plan)
	require.NoError(t, err)
	require.NoError(t, clone.Drop(ctx))
}

type pruneBeforeDrop struct {
	postgresfixture.Backend
	other postgresfixture.Fixture
}

func (b pruneBeforeDrop) Drop(ctx context.Context, conn *sql.Conn, name string) error {
	// Another pruner skips our locked stage and can remove the other one.
	if _, err := b.other.Prune(ctx, time.Hour); err != nil {
		return err
	}
	return b.Backend.Drop(ctx, conn, name)
}

func TestConcurrentPrunesReportOnlyTheirOwnDrops(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	pool := postgresfixture.Fixture{Admin: admin, Backend: &installer{dsn: dsn}}
	var stages []string
	for range 2 {
		p := postgresfixture.Installation{Revision: t.TempDir()}
		template, err := p.TemplateName()
		require.NoError(t, err)
		stage := "tabula_stg_" + strings.TrimPrefix(template, "tabula_tpl_")
		quoted := pq.QuoteIdentifier(stage)
		_, err = admin.ExecContext(ctx, "CREATE DATABASE "+quoted+" TEMPLATE template0")
		require.NoError(t, err)
		t.Cleanup(func() { _, err := admin.Exec("DROP DATABASE IF EXISTS " + quoted); require.NoError(t, err) })
		stages = append(stages, stage)
	}
	f := postgresfixture.Fixture{Admin: admin, Backend: pruneBeforeDrop{pool.Backend, pool}}
	dropped, err := f.Prune(ctx, time.Hour)
	require.NoError(t, err)
	require.Len(t, dropped, 1)
	require.Contains(t, stages, dropped[0])
	for _, stage := range stages {
		var present bool
		require.NoError(t, admin.QueryRowContext(ctx, "SELECT EXISTS (SELECT FROM pg_database WHERE datname=$1)", stage).Scan(&present))
		require.False(t, present)
	}
}
