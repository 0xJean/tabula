//go:build integration

package sqlcheck

import (
	"database/sql"
	"os"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestPrepareTypedReadDescription(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(ctx, "CREATE TEMP TABLE items (id bigint, label text)")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "CREATE TEMP SEQUENCE calls")
	require.NoError(t, err)
	for _, test := range []struct {
		query string
		shape tabula.StatementShape
		want  string
	}{
		{"SELECT $1::bigint AS id", tabula.StatementShape{Inputs: []tabula.ShapeInput{{Type: "integer"}}}, "input"},
		{"SELECT $1::bigint AS id", tabula.StatementShape{}, "inputs"},
		{"SELECT id FROM pg_temp.items", tabula.StatementShape{PhysicalColumns: []tabula.ShapeColumn{{Relation: "pg_temp.items", Name: "id", Type: "text"}}}, "physical column"},
		{"SELECT id FROM pg_temp.items", tabula.StatementShape{Result: []tabula.ShapeField{{Name: "id", Type: "text"}}}, "result types"},
		{"SELECT id,label FROM pg_temp.items", tabula.StatementShape{Result: []tabula.ShapeField{{Name: "id", Type: "bigint"}}}, "result fields"},
		{"SELECT id,label FROM pg_temp.items", tabula.StatementShape{Result: []tabula.ShapeField{{Name: "label", Type: "text"}, {Name: "id", Type: "bigint"}}}, "result fields"},
		{"SELECT nextval('pg_temp.calls') AS id", tabula.StatementShape{Result: []tabula.ShapeField{{Name: "id", Type: "bigint"}}}, ""},
	} {
		err := prepareShape(ctx, tx, test.query, "", &test.shape)
		if test.want == "" {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, test.want)
		}
	}
	var called bool
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT is_called FROM pg_temp.calls").Scan(&called))
	require.False(t, called)
	var count int
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT count(*) FROM pg_catalog.pg_prepared_statements WHERE name LIKE 'tabula_check_%'").Scan(&count))
	require.Zero(t, count)
}
