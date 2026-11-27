//go:build integration

package pgxdriver_test

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/sqlcheck"
)

func TestVerifyWriteResultDescriptions(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	ns := "tabula_description_" + strings.ToLower(rand.Text())
	exec := func(q string) { t.Helper(); _, err := pool.Exec(ctx, q); require.NoError(t, err, q) }
	exec("CREATE SCHEMA " + ns)
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA "+ns+" CASCADE")
		require.NoError(t, err)
	})
	exec("CREATE DOMAIN " + ns + ".kind AS text CHECK (VALUE <> '')")
	exec("CREATE SEQUENCE " + ns + ".entries")
	exec("CREATE TABLE " + ns + ".items(id bigint DEFAULT nextval('" + ns + ".entries'), note " + ns + ".kind)")
	exec("CREATE FUNCTION " + ns + ".append_note(v text) RETURNS bigint LANGUAGE plpgsql VOLATILE AS $$ BEGIN INSERT INTO " + ns + ".items(note) VALUES(v); RETURN 1; END $$")
	verify := func(query string, shape tabula.StatementShape) ([]string, error) {
		return sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), []tabula.Definition{{Name: "items", Statements: map[string]string{"probe": query}, StatementShapes: map[string]tabula.StatementShape{"probe": shape}}}, "")
	}
	domain := tabula.StatementShape{Effects: tabula.EffectWrite, Inputs: []tabula.ShapeInput{{Name: "note", Type: ns + ".kind"}}, Result: []tabula.ShapeField{{Name: "note", Type: ns + ".kind"}}, ResultLineage: []tabula.ResultLineage{{Relation: ns + ".items", Column: "note"}}}
	insert := "INSERT INTO " + ns + ".items(note) VALUES(CAST($1 AS " + ns + ".kind)) RETURNING note"
	findings, err := verify(insert, domain)
	require.NoError(t, err)
	require.Empty(t, findings)
	computed := domain
	computed.ResultLineage = []tabula.ResultLineage{{CastType: ns + ".kind"}}
	findings, err = verify(insert[:len(insert)-len("note")]+"CAST(note AS "+ns+".kind) AS note", computed)
	require.NoError(t, err)
	require.Empty(t, findings)
	findings, err = verify(insert[:len(insert)-len("note")]+"CAST(note AS pg_catalog.text) AS note", computed)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	require.Contains(t, findings[0], "logical domain")
	findings, err = verify("WITH added AS ("+insert+") SELECT note FROM added", domain)
	require.NoError(t, err)
	require.Empty(t, findings)
	// A cast in just one set arm does not prove the combined result's domain.
	findings, err = verify("WITH added AS ("+insert+") SELECT CAST(note AS "+ns+".kind) AS note FROM added UNION ALL SELECT 'plain'::text", computed)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	require.Contains(t, findings[0], "logical domain")
	function := tabula.StatementShape{Effects: tabula.EffectWrite, Inputs: []tabula.ShapeInput{{Name: "note", Type: "pg_catalog.text"}}, Result: []tabula.ShapeField{{Name: "id", Type: "pg_catalog.int8"}}}
	findings, err = verify("SELECT "+ns+".append_note(CAST($1 AS text)) AS id", function)
	require.NoError(t, err)
	require.Empty(t, findings)
	var count int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+ns+".items").Scan(&count))
	require.Zero(t, count)
	var used bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT is_called FROM "+ns+".entries").Scan(&used))
	require.False(t, used)
}

func TestVerifyConflictArbiters(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	ns := "tabula_arbiter_" + strings.ToLower(rand.Text())
	exec := func(q string) { t.Helper(); _, err := pool.Exec(ctx, q); require.NoError(t, err, q) }
	exec("CREATE SCHEMA " + ns)
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA "+ns+" CASCADE")
		require.NoError(t, err)
	})
	items := ns + ".items"
	exec("CREATE TABLE " + items + " (id integer, rank integer, note text)")
	exec("CREATE UNIQUE INDEX active_items ON " + items + " (id) WHERE rank > 0")
	shape := tabula.StatementShape{Effects: tabula.EffectWrite, Inputs: []tabula.ShapeInput{{Type: "pg_catalog.int4"}}, Conflicts: []tabula.ConflictRequirement{{Relation: items, Columns: []string{"id"}, DoUpdate: true}}}
	check := func(query string, s tabula.StatementShape) []string {
		t.Helper()
		findings, err := sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), []tabula.Definition{{Name: "items", Statements: map[string]string{"write": query}, StatementShapes: map[string]tabula.StatementShape{"write": s}}}, "")
		require.NoError(t, err)
		return findings
	}
	insert := "INSERT INTO " + items + "(id,rank) VALUES(CAST($1 AS integer),1) ON CONFLICT(id) WHERE rank > 0 DO UPDATE SET note='ready'"
	require.Empty(t, check(insert, shape))
	require.NotEmpty(t, check(strings.Replace(insert, "rank > 0", "rank > $1", 1), shape))
	wrong := shape
	wrong.Conflicts = []tabula.ConflictRequirement{{Relation: items, Columns: []string{"rank"}, DoUpdate: true}}
	require.NotEmpty(t, check(insert, wrong))
	notes := ns + ".notes"
	exec("CREATE TABLE " + notes + " (value int4range, CONSTRAINT notes_disjoint EXCLUDE USING gist (value WITH &&))")
	exclusion := tabula.StatementShape{Effects: tabula.EffectWrite, Conflicts: []tabula.ConflictRequirement{{Relation: notes, Constraint: "notes_disjoint"}}}
	require.Empty(t, check("INSERT INTO "+notes+" VALUES(int4range(1,2)) ON CONFLICT ON CONSTRAINT notes_disjoint DO NOTHING", exclusion))
	var count int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+items).Scan(&count))
	require.Zero(t, count)
}
