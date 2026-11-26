//go:build integration

package sqlcheck

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestReadinessResultKeyCatalog(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ns := fmt.Sprintf("key_proof_%d", time.Now().UnixNano())
	role := ns + "_reader"
	exec := func(text string) {
		t.Helper()
		_, err := db.ExecContext(t.Context(), text)
		require.NoError(t, err, text)
	}
	exec("CREATE ROLE " + role)
	t.Cleanup(func() { _, err := db.Exec("DROP ROLE " + role); require.NoError(t, err) })
	exec("CREATE SCHEMA " + ns)
	t.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + ns + " CASCADE"); require.NoError(t, err) })
	exec("GRANT USAGE ON SCHEMA " + ns + " TO " + role)
	exec("CREATE TABLE " + ns + ".items (id bigint PRIMARY KEY, label text)")
	exec("GRANT SELECT(id) ON " + ns + ".items TO " + role)
	needs := ReadinessNeeds{Keys: []tabula.KeyRequirement{{Relation: ns + ".items", Columns: []string{"id"}}}, Comparisons: []tabula.ComparisonRequirement{{Relation: ns + ".items", Column: "id"}}}
	check := func(want int) {
		t.Helper()
		missing, err := CheckReadiness(t.Context(), db, role, needs)
		require.NoError(t, err)
		require.Len(t, missing, want, "%v", missing)
	}
	check(0)
	defs := []tabula.Definition{{Name: "entries", Statements: map[string]string{"page": "SELECT id FROM " + ns + ".items"}, StatementQueries: map[string]string{"page": "page"}, StatementShapes: map[string]tabula.StatementShape{"page": {Effects: tabula.EffectRead, Keys: needs.Keys, Comparisons: needs.Comparisons, Result: []tabula.ShapeField{{Name: "id", Type: "pg_catalog.int8"}}}}}}
	verify := func(want int) {
		t.Helper()
		failures, err := VerifyStatements(t.Context(), db, defs, role)
		require.NoError(t, err)
		require.Len(t, failures, want, "%v", failures)
	}
	verify(0)
	exec("ALTER TABLE " + ns + ".items DROP CONSTRAINT items_pkey")
	check(1)
	verify(1)
	exec("ALTER TABLE " + ns + ".items ADD PRIMARY KEY (id) DEFERRABLE INITIALLY IMMEDIATE")
	check(1)
	verify(1)
	exec("ALTER TABLE " + ns + ".items DROP CONSTRAINT items_pkey")
	exec("ALTER TABLE " + ns + ".items ADD PRIMARY KEY (id)")
	exec("CREATE TABLE " + ns + ".notes () INHERITS (" + ns + ".items)")
	exec("INSERT INTO " + ns + ".items VALUES (1,'parent')")
	exec("INSERT INTO " + ns + ".notes VALUES (1,'child')")
	check(1)
	verify(1)
	exec("DROP TABLE " + ns + ".notes")
	check(0)
	exec("CREATE TABLE " + ns + ".groups (region int, id bigint, PRIMARY KEY(region,id)) PARTITION BY RANGE(region)")
	exec("CREATE TABLE " + ns + ".regions PARTITION OF " + ns + ".groups FOR VALUES FROM (0) TO (10)")
	needs = ReadinessNeeds{Keys: []tabula.KeyRequirement{{Relation: ns + ".groups", Columns: []string{"id", "region"}}}}
	check(0)
	needs.Keys[0].Columns = []string{"id"}
	check(1)
	needs.Keys[0].Columns = []string{"region", "id"}
	check(0)
	exec("CREATE COLLATION " + ns + ".folded (provider=icu,locale='und',deterministic=false)")
	exec("CREATE TABLE " + ns + ".notes (label text COLLATE " + ns + ".folded PRIMARY KEY)")
	needs = ReadinessNeeds{Keys: []tabula.KeyRequirement{{Relation: ns + ".notes", Columns: []string{"label"}}}, Comparisons: []tabula.ComparisonRequirement{{Relation: ns + ".notes", Column: "label"}}}
	check(2)
	needs = ReadinessNeeds{Comparisons: []tabula.ComparisonRequirement{{Relation: ns + ".items", Column: "label"}}}
	check(0) // comparison evidence requires no SELECT on the column
	exec("ALTER TABLE " + ns + `.items ADD COLUMN label_c text COLLATE "C", ADD COLUMN label_posix text COLLATE "POSIX"`)
	needs.Comparisons = []tabula.ComparisonRequirement{{Relation: ns + ".items", Column: "label_c"}, {Relation: ns + ".items", Column: "label_posix"}}
	check(0) // independent ordering terms need not use the same collation
	needs.Comparisons = []tabula.ComparisonRequirement{{Relation: ns + ".items", Column: "label_c", WithRelation: ns + ".items", WithColumn: "label_posix"}}
	check(1)
	shape := defs[0].StatementShapes["page"]
	shape.Comparisons = needs.Comparisons
	defs[0].StatementShapes["page"] = shape
	path := filepath.Join(t.TempDir(), "statements.sql")
	require.NoError(t, os.WriteFile(path, []byte(RenderAllSnapshot(defs, nil)), 0600))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	defs, err = ParseSnapshot(string(data))
	require.NoError(t, err)
	work := RoleStatements{}
	work.AddDefinitions(role, defs...)
	needs, err = work.Readiness(role)
	require.NoError(t, err)
	check(1)
	verify(1) // rejects the proof even when the SQL does not compare the columns
	needs = ReadinessNeeds{Comparisons: []tabula.ComparisonRequirement{{Relation: ns + ".items", Column: "label", WithRelation: ns + ".items", WithColumn: "label_c"}}}
	check(0) // a non-default implicit collation wins over the default
	needs.Comparisons[0].WithColumn = "label"
	check(0)
	needs.Comparisons[0].WithRelation, needs.Comparisons[0].WithColumn = ns+".notes", "label"
	check(1) // a nested nondeterministic input remains unproved
}
