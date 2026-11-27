//go:build integration

package sqlcheck

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestGranularityTableIntegration(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn, "TABULA_TEST_DSN is required for integration tests")
	ctx := t.Context()

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	ns := fmt.Sprintf("tabula_gran_%d", time.Now().UnixNano())
	reader := ns + "_reader"
	colOnly := ns + "_colonly"
	t.Cleanup(func() {
		_, _ = db.Exec("DROP SCHEMA IF EXISTS " + ns + " CASCADE")
		_, _ = db.Exec("DROP ROLE IF EXISTS " + reader)
		_, _ = db.Exec("DROP ROLE IF EXISTS " + colOnly)
	})

	for _, q := range []string{
		"CREATE ROLE " + reader + " NOLOGIN",
		"CREATE ROLE " + colOnly + " NOLOGIN",
		"CREATE SCHEMA " + ns,
		"CREATE SEQUENCE " + ns + ".item_seq",
		"CREATE TABLE " + ns + ".items (id bigint PRIMARY KEY, name text NOT NULL, note text)",
		"CREATE TABLE " + ns + ".notes (id bigint PRIMARY KEY, body text)",
		"CREATE FUNCTION " + ns + ".ping(int4) RETURNS int4 LANGUAGE sql AS $$ SELECT $1 $$",
		"REVOKE ALL ON FUNCTION " + ns + ".ping(int4) FROM PUBLIC",
		"GRANT USAGE ON SCHEMA " + ns + " TO " + reader + ", " + colOnly,
		// reader has table-level SELECT, UPDATE, DELETE on items, sequence USAGE, function EXECUTE,
		// PLUS an explicit column-level SELECT(id) on notes.
		"GRANT SELECT, UPDATE, DELETE ON " + ns + ".items TO " + reader,
		"GRANT SELECT(id) ON " + ns + ".notes TO " + reader,
		"GRANT USAGE ON SEQUENCE " + ns + ".item_seq TO " + reader,
		"GRANT EXECUTE ON FUNCTION " + ns + ".ping(int4) TO " + reader,
		// colOnly has ONLY column-level grants (no table-level grants).
		"GRANT SELECT(id, name), INSERT(id, name), UPDATE(name) ON " + ns + ".items TO " + colOnly,
	} {
		_, err := db.ExecContext(ctx, q)
		require.NoError(t, err, q)
	}

	// 1. ReadCatalog: GranularityAll includes COLUMN grants; GranularityTable omits COLUMN grants while keeping TABLE, SEQUENCE, FUNCTION.
	allScope := CatalogScope{Schemas: []string{ns}, Roles: []string{reader}}
	allCat, err := ReadCatalog(ctx, db, allScope)
	require.NoError(t, err)
	var hasColAll bool
	for _, g := range allCat.Grants {
		if g.Kind == "COLUMN" {
			hasColAll = true
		}
	}
	require.True(t, hasColAll, "GranularityAll must include COLUMN grants")

	tblScope := CatalogScope{Schemas: []string{ns}, Roles: []string{reader}, Granularity: GranularityTable}
	tblCat, err := ReadCatalog(ctx, db, tblScope)
	require.NoError(t, err)
	kinds := map[string]bool{}
	for _, g := range tblCat.Grants {
		kinds[g.Kind] = true
		require.NotEqual(t, "COLUMN", g.Kind, "GranularityTable must exclude COLUMN grants")
	}
	require.True(t, kinds["TABLE"])
	require.True(t, kinds["SEQUENCE"])
	require.True(t, kinds["FUNCTION"])

	// 2. CheckGrantManifest under GranularityTable succeeds against table/sequence/function manifest even though column grants exist.
	diff, err := CheckGrantManifest(ctx, db, GrantManifest{
		CatalogScope: tblScope,
		Grants:       tblCat.Grants,
	})
	require.NoError(t, err)
	require.Empty(t, diff.Missing)
	require.Empty(t, diff.Extra)

	// Rejects COLUMN grant inside a GranularityTable manifest.
	_, err = CheckGrantManifest(ctx, db, GrantManifest{
		CatalogScope: tblScope,
		Grants: append(append([]Grant{}, tblCat.Grants...), Grant{
			Role: reader, Schema: ns, Kind: "COLUMN", Object: "notes", Column: "id", Privilege: "SELECT",
		}),
	})
	require.ErrorContains(t, err, "GranularityTable")

	// 3. MinimizeGrants under GranularityTable trials only table/sequence/function grants (keeps TABLE SELECT, removes unused UPDATE/DELETE, ignores COLUMN ACLs).
	minRes, err := MinimizeGrants(ctx, db, MinimizeConfig{
		Role:        reader,
		Schemas:     []string{ns},
		Granularity: GranularityTable,
		Statements: []Statement{
			{Name: "read_items", SQL: "SELECT id, name FROM " + ns + ".items WHERE id = $1"},
		},
	})
	require.NoError(t, err)
	for _, g := range minRes.Kept {
		require.NotEqual(t, "COLUMN", g.Kind)
	}
	for _, g := range minRes.Removed {
		require.NotEqual(t, "COLUMN", g.Kind)
	}
	require.Contains(t, minRes.Kept, Grant{Role: reader, Schema: ns, Kind: "TABLE", Object: "items", Privilege: "SELECT"})
	require.Contains(t, minRes.Removed, Grant{Role: reader, Schema: ns, Kind: "TABLE", Object: "items", Privilege: "UPDATE"})
	require.Contains(t, minRes.Removed, Grant{Role: reader, Schema: ns, Kind: "TABLE", Object: "items", Privilege: "DELETE"})

	// 4. CheckReadiness TableSelect / TableInsert / TableUpdate / TableDelete use has_table_privilege.
	items := ns + ".items"
	missing, err := CheckReadiness(ctx, db, reader, ReadinessNeeds{
		SchemaUsage: []string{ns},
		TableSelect: []string{items},
		TableUpdate: []string{items},
		TableDelete: []string{items},
	})
	require.NoError(t, err)
	require.Empty(t, missing)

	// Missing TableInsert on reader is reported.
	missing, err = CheckReadiness(ctx, db, reader, ReadinessNeeds{
		TableSelect: []string{items},
		TableInsert: []string{items},
	})
	require.NoError(t, err)
	require.Equal(t, []MissingRequirement{{Kind: "TABLE", Object: items, Privilege: "INSERT"}}, missing)

	// Column-only grants do NOT satisfy TableSelect / TableInsert / TableUpdate (proving has_table_privilege is checked).
	missing, err = CheckReadiness(ctx, db, colOnly, ReadinessNeeds{
		TableSelect: []string{items},
		TableInsert: []string{items},
		TableUpdate: []string{items},
		TableDelete: []string{items},
	})
	require.NoError(t, err)
	require.Equal(t, []MissingRequirement{
		{Kind: "TABLE", Object: items, Privilege: "DELETE"},
		{Kind: "TABLE", Object: items, Privilege: "INSERT"},
		{Kind: "TABLE", Object: items, Privilege: "SELECT"},
		{Kind: "TABLE", Object: items, Privilege: "UPDATE"},
	}, missing)
}
