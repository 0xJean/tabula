//go:build integration

package sqlcheck

import (
	"context"
	"crypto/rand"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func writeCatalog(t *testing.T) (*sql.DB, string, string, func(string)) {
	t.Helper()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ns := "tabula_catalog_" + strings.ToLower(rand.Text())
	role := ns + "_r"
	exec := func(q string) { t.Helper(); _, err := db.ExecContext(t.Context(), q); require.NoError(t, err, q) }
	cleanup := func(q string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		_, err := db.ExecContext(ctx, q)
		require.NoError(t, err, q)
	}
	exec("CREATE ROLE " + role + " NOLOGIN")
	t.Cleanup(func() { cleanup("DROP ROLE " + role) })
	exec("CREATE SCHEMA " + ns)
	t.Cleanup(func() { cleanup("DROP SCHEMA " + ns + " CASCADE") })
	exec("GRANT USAGE ON SCHEMA " + ns + " TO " + role)
	return db, ns, role, exec
}

func TestWriteDefaultReadiness(t *testing.T) {
	db, ns, role, exec := writeCatalog(t)
	items := ns + ".items"
	exec("CREATE SEQUENCE " + ns + ".entries")
	exec("CREATE TABLE " + items + " (id bigint GENERATED ALWAYS AS IDENTITY, serial_id bigserial, note bigint DEFAULT nextval('" + ns + ".entries'), rank integer DEFAULT 2)")
	check := func(columns ...string) []MissingRequirement {
		t.Helper()
		got, err := CheckReadiness(t.Context(), db, role, ReadinessNeeds{Defaults: []tabula.DefaultRequirement{{Relation: items, Columns: columns}}})
		require.NoError(t, err)
		return got
	}
	require.Empty(t, check("id", "rank"))
	omitted := ReadinessNeeds{Defaults: []tabula.DefaultRequirement{{Relation: items, IncludesOmitted: true, InsertColumns: []string{"note", "rank"}}}}
	missing, err := CheckReadiness(t.Context(), db, role, omitted)
	require.NoError(t, err)
	require.Equal(t, []MissingRequirement{{"SEQUENCE", items, "serial_id", "USAGE"}}, missing)
	omitted.Defaults[0].InsertColumns = append(omitted.Defaults[0].InsertColumns, "serial_id")
	missing, err = CheckReadiness(t.Context(), db, role, omitted)
	require.NoError(t, err)
	require.Empty(t, missing)
	require.Equal(t, []MissingRequirement{{"SEQUENCE", items, "note", "USAGE"}, {"SEQUENCE", items, "serial_id", "USAGE"}}, check("serial_id", "note"))
	require.Equal(t, []MissingRequirement{{"COLUMN", items, "absent", "EXISTS"}}, check("absent"))
	exec("GRANT USAGE ON SEQUENCE " + ns + ".entries TO " + role)
	require.Empty(t, check("note"))
	// Ownership survives this change, but the default no longer uses a sequence.
	exec("ALTER TABLE " + items + " ALTER COLUMN serial_id SET DEFAULT 8")
	require.Empty(t, check("serial_id"))
	exec("REVOKE USAGE ON SEQUENCE " + ns + ".entries FROM " + role)
	require.Len(t, check("note"), 1)
	var used bool
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT is_called FROM "+ns+".entries").Scan(&used))
	require.False(t, used)
}

func TestWriteUniqueMatchReadiness(t *testing.T) {
	db, ns, role, exec := writeCatalog(t)
	items := ns + ".items"
	exec("CREATE TABLE " + items + " (id bigint PRIMARY KEY, note integer NOT NULL, region integer NOT NULL, label integer, delayed integer NOT NULL UNIQUE DEFERRABLE, partial integer NOT NULL)")
	exec("CREATE UNIQUE INDEX notes_unique ON " + items + " (note,region) INCLUDE (label)")
	exec("CREATE UNIQUE INDEX labels_unique ON " + items + " (label)")
	exec("CREATE UNIQUE INDEX partial_unique ON " + items + " (partial) WHERE partial > 0")
	check := func(columns ...string) []MissingRequirement {
		t.Helper()
		got, err := CheckReadiness(t.Context(), db, role, ReadinessNeeds{UniqueMatches: []tabula.UniqueMatchRequirement{{Relation: items, Columns: columns}}})
		require.NoError(t, err)
		return got
	}
	require.Empty(t, check("id"))
	require.Empty(t, check("region", "note"))
	require.Empty(t, check("region", "note", "label"))
	for _, columns := range [][]string{{"note"}, {"label"}, {"delayed"}, {"partial"}, {"missing"}, {"id", "missing"}} {
		require.Len(t, check(columns...), 1)
	}
	exec("CREATE TABLE " + ns + ".notes () INHERITS (" + items + ")")
	require.Len(t, check("id"), 1)
	exec("DROP TABLE " + ns + ".notes")
	exec("DROP INDEX " + ns + ".notes_unique")
	require.Len(t, check("note", "region"), 1)
}

func TestWriteConflictReadiness(t *testing.T) {
	db, ns, role, exec := writeCatalog(t)
	items, notes := ns+".items", ns+".notes"
	exec("CREATE TABLE " + items + " (id integer PRIMARY KEY, rank integer, delayed integer UNIQUE DEFERRABLE)")
	exec("CREATE UNIQUE INDEX ranked_items ON " + items + " (rank) WHERE rank > 0")
	exec("CREATE TABLE " + notes + " (value int4range, CONSTRAINT notes_disjoint EXCLUDE USING gist (value WITH &&))")
	check := func(r tabula.ConflictRequirement) []MissingRequirement {
		t.Helper()
		got, err := CheckReadiness(t.Context(), db, role, ReadinessNeeds{Conflicts: []tabula.ConflictRequirement{r}})
		require.NoError(t, err)
		return got
	}
	require.Empty(t, check(tabula.ConflictRequirement{Relation: items, Columns: []string{"id"}, DoUpdate: true}))
	require.Empty(t, check(tabula.ConflictRequirement{Relation: items, Columns: []string{"rank"}}))
	require.Len(t, check(tabula.ConflictRequirement{Relation: items, Columns: []string{"delayed"}}), 1)
	require.Len(t, check(tabula.ConflictRequirement{Relation: items, Constraint: "missing"}), 1)
	require.Empty(t, check(tabula.ConflictRequirement{Relation: notes, Constraint: "notes_disjoint"}))
	require.Len(t, check(tabula.ConflictRequirement{Relation: notes, Constraint: "notes_disjoint", DoUpdate: true}), 1)
	// A targetless DO NOTHING may not silently ignore deferrable arbiters.
	require.Len(t, check(tabula.ConflictRequirement{Relation: items}), 1)
	exec("ALTER TABLE " + items + " DROP CONSTRAINT items_delayed_key")
	require.Empty(t, check(tabula.ConflictRequirement{Relation: items}))
}

func TestWriteDescriptionRequiresProtocol(t *testing.T) {
	db, ns, role, exec := writeCatalog(t)
	items := ns + ".items"
	exec("CREATE TABLE " + items + "(id bigserial, note text)")
	exec("GRANT INSERT(note), SELECT(id) ON " + items + " TO " + role)
	exec("GRANT USAGE ON ALL SEQUENCES IN SCHEMA " + ns + " TO " + role)
	insert := "INSERT INTO " + items + "(note) VALUES(CAST($1 AS text)) RETURNING id"
	shape := tabula.StatementShape{Effects: tabula.EffectWrite, Inputs: []tabula.ShapeInput{{Type: "pg_catalog.text"}}, Result: []tabula.ShapeField{{Name: "id", Type: "pg_catalog.int8"}}}
	for _, query := range []string{insert, "WITH added AS (" + insert + ") SELECT id FROM added"} {
		defs := []tabula.Definition{{Name: "items", Statements: map[string]string{"probe": query}, StatementShapes: map[string]tabula.StatementShape{"probe": shape}}}
		_, err := VerifyStatements(t.Context(), db, defs, role)
		require.ErrorIs(t, err, ErrDescribeRequired)
		// A visible mutation is still unsafe if its snapshot claims to read.
		wrong := shape
		wrong.Effects = tabula.EffectRead
		defs[0].StatementShapes["probe"] = wrong
		_, err = VerifyStatements(t.Context(), db, defs, role)
		require.ErrorIs(t, err, ErrDescribeRequired)
	}
	shape.Result = nil
	countOnly := strings.TrimSuffix(insert, " RETURNING id")
	defs := []tabula.Definition{{Name: "items", Statements: map[string]string{"probe": countOnly}, StatementShapes: map[string]tabula.StatementShape{"probe": shape}}}
	findings, err := VerifyStatements(t.Context(), db, defs, role)
	require.NoError(t, err)
	require.Empty(t, findings)
	var used bool
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT is_called FROM "+ns+".items_id_seq").Scan(&used))
	require.False(t, used)
}
