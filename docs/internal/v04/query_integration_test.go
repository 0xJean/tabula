//go:build integration

package v04

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/0xJean/tabula/stdsql"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestComposedQueryPostgres(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn, "set TABULA_TEST_DSN to a disposable PostgreSQL database")
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(ctx, `
		CREATE TEMP TABLE labels (id bigint PRIMARY KEY, caption text NOT NULL);
		CREATE TEMP TABLE notes (id bigint PRIMARY KEY, scope_id bigint NOT NULL, rank bigint NOT NULL,
			due_at date NOT NULL, label_id bigint, title text, code text, visible boolean, pinned boolean);
		INSERT INTO labels VALUES (1, 'blue');
		INSERT INTO notes VALUES
			(1, 7, 5, '2020-01-01', 1, 'needle', '', true, false),
			(2, 7, 5, '2030-01-01', NULL, '', 'needle', false, true),
			(3, 8, 5, '2020-01-01', 1, 'needle', '', true, false),
			(4, 7, 5, '2020-01-01', 1, 'needle', '', false, false),
			(5, 7, 4, '2020-01-01', 1, 'other', '', true, false)`)
	require.NoError(t, err)
	q := noteQuery(newRegistry[int64](false, SessionKey{}))
	for key, statement := range q.view.Definition().Statements {
		require.NoError(t, sqlcheck.PrepareStatement(ctx, tx, statement, ""), key)
	}
	policy := stdsql.ScopePolicy[int64]{IsUnscoped: func(s int64) bool { return s == 0 }}
	rows := q.Bind(stdsql.NewSession(tx, int64(7), policy))
	in := inputs{asOf: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), showLabels: true}

	t.Run("one search value across columns and scopes", func(t *testing.T) {
		search := in
		search.search, search.pattern = true, "%needle%"
		got, total, err := rows.List(t.Context(), search, 2, 0, tabula.CountExact)
		require.NoError(t, err)
		require.EqualValues(t, 2, total)
		require.Equal(t, []note{{2, 5, false, ""}, {1, 5, true, "blue"}}, got)
		all := q.Bind(stdsql.NewSession(tx, int64(0), policy))
		total, err = all.Count(t.Context(), search)
		require.NoError(t, err)
		require.EqualValues(t, 3, total)
		search.pattern = nil
		total, err = rows.Count(t.Context(), search)
		require.NoError(t, err)
		require.Zero(t, total)
	})
	t.Run("projection and join parameters", func(t *testing.T) {
		display := in
		display.showLabels = false
		display.asOf = display.asOf.AddDate(10, 0, 0)
		got, total, err := rows.List(t.Context(), display, 2, 0, tabula.CountNone)
		require.NoError(t, err)
		require.EqualValues(t, -1, total)
		require.Equal(t, []note{{2, 5, true, ""}, {1, 5, true, ""}}, got)
	})
	t.Run("optimized count and stable cursor", func(t *testing.T) {
		first, err := rows.ListKeyset(t.Context(), in, 1, nil)
		require.NoError(t, err)
		require.True(t, first.HasMore)
		require.Equal(t, []note{{2, 5, false, ""}}, first.Items)
		next, err := rows.ListKeyset(t.Context(), in, 2, first.Items[0].cursor())
		require.NoError(t, err)
		require.False(t, next.HasMore)
		require.Equal(t, []note{{1, 5, true, "blue"}, {5, 4, true, "blue"}}, next.Items)
		for row, err := range rows.Iter(t.Context(), in) {
			require.NoError(t, err)
			require.Equal(t, first.Items[0], row)
			break
		}
		full, err := rows.Find(t.Context(), in)
		require.NoError(t, err)
		total, err := rows.Count(t.Context(), in)
		require.NoError(t, err)
		require.EqualValues(t, len(full), total)
		require.Equal(t, append(first.Items, next.Items...), full)
	})
}
