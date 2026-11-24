//go:build integration

package sqlcheck

import (
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestAuthorizationWorkflow(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ns := fmt.Sprintf("tabula_audit_%d", time.Now().UnixNano())
	reader, writer, group := ns+"_reader", ns+"_writer", ns+"_group"
	tbl := ns + ".items"
	t.Cleanup(func() {
		_, _ = db.Exec("DROP SCHEMA IF EXISTS " + ns + " CASCADE")
		for _, r := range []string{reader, writer, group} {
			_, _ = db.Exec("DROP ROLE IF EXISTS " + r)
		}
	})
	for _, q := range []string{
		"CREATE SCHEMA " + ns, "CREATE ROLE " + reader, "CREATE ROLE " + writer, "CREATE ROLE " + group, "GRANT " + group + " TO " + reader,
		"CREATE TABLE " + tbl + " (id integer PRIMARY KEY, scope text, note text)",
		"INSERT INTO " + tbl + " VALUES (1,'a',NULL),(2,'b','two')",
		"CREATE SEQUENCE " + ns + ".seq", "CREATE FUNCTION " + ns + ".f(integer) RETURNS integer LANGUAGE SQL SECURITY DEFINER AS 'SELECT $1'",
		"CREATE FUNCTION " + ns + ".f(text) RETURNS text LANGUAGE SQL SECURITY DEFINER AS 'SELECT $1'",
		"CREATE PROCEDURE " + ns + ".refresh_notes() LANGUAGE plpgsql SECURITY DEFINER AS 'BEGIN NULL; END;'",
		"GRANT USAGE ON SCHEMA " + ns + " TO " + reader + "," + writer,
		"GRANT SELECT, UPDATE ON " + tbl + " TO " + reader, "GRANT SELECT(id) ON " + tbl + " TO " + reader,
		"GRANT SELECT, INSERT, UPDATE ON " + tbl + " TO " + writer, "GRANT USAGE ON SEQUENCE " + ns + ".seq TO " + reader,
		"GRANT EXECUTE ON FUNCTION " + ns + ".f(integer) TO " + reader,
		"GRANT EXECUTE ON PROCEDURE " + ns + ".refresh_notes() TO " + reader,
		"ALTER TABLE " + tbl + " ENABLE ROW LEVEL SECURITY", "ALTER TABLE " + tbl + " FORCE ROW LEVEL SECURITY",
		"CREATE POLICY visible ON " + tbl + " FOR SELECT TO " + group + " USING (scope=current_setting('example.scope',true))",
		"CREATE POLICY write_scope ON " + tbl + " FOR INSERT TO " + writer + " WITH CHECK (scope=current_setting('example.scope',true))",
		"CREATE POLICY fence ON " + tbl + " AS RESTRICTIVE FOR ALL TO PUBLIC USING (true)",
	} {
		_, err := db.ExecContext(ctx, q)
		require.NoError(t, err, q)
	}
	scope := CatalogScope{[]string{ns}, []string{reader, writer, "PUBLIC"}}
	cat, err := ReadCatalog(ctx, db, scope)
	require.NoError(t, err)
	require.Len(t, cat.Policies, 3)
	require.Equal(t, []Relation{{ns, "items"}}, cat.ForceRLS)
	require.Len(t, cat.Definers, 3)
	require.ElementsMatch(t, []string{"pg_catalog.int4", "pg_catalog.text"}, []string{cat.Definers[0].Arguments, cat.Definers[1].Arguments})
	require.Equal(t, "refresh_notes", cat.Definers[2].Name)
	require.Contains(t, cat.Definers[2].Definition, "CREATE OR REPLACE PROCEDURE")
	require.Contains(t, cat.Snapshots()["security-definers.json"], "refresh_notes")
	require.False(t, slices.ContainsFunc(cat.Grants, func(g Grant) bool { return g.Object == "refresh_notes" }))
	require.Contains(t, cat.Snapshots()["policies.json"], "example.scope")
	again, err := ReadCatalog(ctx, db, scope)
	require.NoError(t, err)
	require.Equal(t, cat.Snapshots(), again.Snapshots())
	publicExec := Grant{Role: "PUBLIC", Schema: ns, Kind: "FUNCTION", Object: "f", Arguments: "pg_catalog.text", Privilege: "EXECUTE"}
	seq := Grant{Role: reader, Schema: ns, Kind: "SEQUENCE", Object: "seq", Privilege: "USAGE"}
	require.Contains(t, cat.Grants, publicExec)
	require.Contains(t, cat.Grants, seq)
	manifest := GrantManifest{scope, cat.Grants}
	diff, err := CheckGrantManifest(ctx, db, manifest)
	require.NoError(t, err)
	require.Empty(t, diff.SQL)
	broad := Grant{Role: reader, Schema: ns, Kind: "TABLE", Object: "items", Privilege: "SELECT"}
	desired := slices.DeleteFunc(slices.Clone(cat.Grants), func(g Grant) bool { return g == publicExec || g == broad })
	more := Grant{Role: writer, Schema: ns, Kind: "COLUMN", Object: "items", Column: "note", Privilege: "UPDATE", GrantOption: true}
	desired = append(desired, more)
	delta, err := CompareGrants(GrantManifest{scope, desired}, cat.Grants)
	require.NoError(t, err)
	require.ElementsMatch(t, []Grant{publicExec, broad}, delta.Extra)
	require.Equal(t, []Grant{more}, delta.Missing)
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for _, q := range delta.SQL {
		_, err := tx.ExecContext(ctx, q)
		require.NoError(t, err, q)
	}
	schemas, roles, err := scope.bind(ctx, tx)
	require.NoError(t, err)
	actual, err := readGrants(ctx, tx, schemas, roles)
	require.NoError(t, err)
	require.ElementsMatch(t, desired, actual)
	require.NoError(t, tx.Rollback())

	get := Statement{Name: "read_id", SQL: "SELECT id FROM " + tbl + " WHERE id=$1"}
	ins := Statement{Name: "add", SQL: "INSERT INTO " + tbl + "(id,scope) VALUES ($1,$2)"}
	work := RoleStatements{reader: {get}, writer: {ins}}
	failures, err := VerifyRoleStatements(ctx, db, work)
	require.NoError(t, err)
	require.Empty(t, failures)
	failures, err = VerifyRoleStatements(ctx, db, RoleStatements{reader: {ins}, writer: {ins}})
	require.NoError(t, err)
	require.Len(t, failures, 1)
	require.Contains(t, failures[0], reader+"/add")
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec("PREPARE app_owned AS SELECT 42")
	require.NoError(t, err)
	require.Error(t, PrepareStatement(ctx, tx, "SELECT missing FROM "+tbl, reader))
	require.NoError(t, PrepareStatement(ctx, tx, get.SQL, reader))
	_, err = tx.Exec("EXECUTE app_owned")
	require.NoError(t, err)
	_, err = tx.Exec("DEALLOCATE app_owned")
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	minCfg := MinimizeConfig{Role: reader, Schemas: scope.Schemas, Statements: []Statement{get}}
	minimum, err := MinimizeGrants(ctx, db, minCfg)
	require.NoError(t, err)
	require.Contains(t, minimum.Removed, broad)
	require.Contains(t, minimum.Unverified, seq)
	require.Contains(t, minimum.Kept, Grant{Role: reader, Schema: ns, Kind: "COLUMN", Object: "items", Column: "id", Privilege: "SELECT"})
	require.NotEmpty(t, minimum.RevokeSQL)
	require.NotEmpty(t, minimum.GrantSQL)
	restored, err := ReadCatalog(ctx, db, scope)
	require.NoError(t, err)
	require.Equal(t, cat, restored)
	minCfg.Keep = []Grant{broad}
	pinned, err := MinimizeGrants(ctx, db, minCfg)
	require.NoError(t, err)
	require.Contains(t, pinned.Kept, broad)
	fexec := Grant{Role: reader, Schema: ns, Kind: "FUNCTION", Object: "f", Arguments: "pg_catalog.int4", Privilege: "EXECUTE"}
	minCfg.Runtime = []RuntimeProbe{
		{BehaviorStatement: BehaviorStatement{SQL: "SELECT nextval($1::regclass)", Args: []any{ns + ".seq"}}, Covers: []Grant{seq}},
		{BehaviorStatement: BehaviorStatement{SQL: "SELECT " + ns + ".f(1)"}, Covers: []Grant{fexec}},
	}
	withRuntime, err := MinimizeGrants(ctx, db, minCfg)
	require.NoError(t, err)
	require.Contains(t, withRuntime.Kept, seq)
	require.Contains(t, withRuntime.Removed, fexec)
	require.Empty(t, withRuntime.Unverified)
	minCfg.Current = []Grant{}
	_, err = MinimizeGrants(ctx, db, minCfg)
	require.ErrorContains(t, err, "differs")
	minCfg.Current = nil
	minCfg.Statements = []Statement{ins}
	_, err = MinimizeGrants(ctx, db, minCfg)
	require.ErrorContains(t, err, "baseline")
	_, err = ReadCatalog(ctx, db, CatalogScope{[]string{ns + "_missing"}, []string{reader}})
	require.Error(t, err)

	r := false
	rules := []PolicyRule{{Name: "read", Roles: []string{group}, Command: "SELECT", Using: "example.scope"}, {Name: "write", Entries: []PolicyID{{Relation{ns, "items"}, "write_scope"}}, Check: "example.scope"}, {Name: "restrictive", Permissive: &r, Roles: []string{"PUBLIC"}}}
	classes, findings, err := ClassifyPolicies(cat.Policies, rules)
	require.NoError(t, err)
	require.Empty(t, findings)
	require.Len(t, classes, 3)
	findings, err = CheckPolicyCoverage(ctx, db, work)
	require.NoError(t, err)
	require.Empty(t, findings)
	t.Run("locking clauses require an update policy", func(t *testing.T) {
		read := "SELECT substring(note FROM 1 FOR 3) FROM " + tbl
		stmts := []Statement{{Name: "substring", SQL: read}}
		var want []string
		for _, lock := range []string{"UPDATE", "NO KEY UPDATE", "SHARE", "KEY SHARE"} {
			name := strings.ReplaceAll(strings.ToLower(lock), " ", "_")
			stmts = append(stmts, Statement{Name: name, SQL: read + " FOR " + lock})
			want = append(want, reader+"/"+name+": "+quoteIdent(ns)+`."items" UPDATE: no applicable permissive policy`)
		}
		work := RoleStatements{reader: stmts}
		failures, err := VerifyRoleStatements(t.Context(), db, work)
		require.NoError(t, err)
		require.Empty(t, failures)
		findings, err := CheckPolicyCoverage(t.Context(), db, work)
		require.NoError(t, err)
		require.ElementsMatch(t, want, findings)
	})
	upsert := ins
	upsert.SQL += " ON CONFLICT (id) DO UPDATE SET scope=excluded.scope RETURNING id"
	findings, err = CheckPolicyCoverage(ctx, db, RoleStatements{writer: {upsert}})
	require.NoError(t, err)
	require.Len(t, findings, 2)
	require.Contains(t, strings.Join(findings, " "), "SELECT")
	require.Contains(t, strings.Join(findings, " "), "UPDATE")
	cte := Statement{Name: "cte", SQL: "WITH q AS (SELECT * FROM " + tbl + ") SELECT id FROM q"}
	findings, err = CheckPolicyCoverage(ctx, db, RoleStatements{reader: {cte}})
	require.NoError(t, err)
	require.Len(t, findings, 1)
	cte.Access = []PolicyAccess{{tbl, "SELECT"}}
	findings, err = CheckPolicyCoverage(ctx, db, RoleStatements{reader: {cte}})
	require.NoError(t, err)
	require.Empty(t, findings)

	matrix := RLSMatrix{Roles: []string{reader, writer}, Scopes: map[string]map[string]string{"a": {"example.scope": "a"}, "b": {"example.scope": "b"}}, Statements: map[string]BehaviorStatement{"read": {SQL: "SELECT id,note FROM " + tbl}, "write": {SQL: ins.SQL, Args: []any{3, "a"}}}}
	a, b, note := "1", "2", "two"
	var one int64 = 1
	for _, role := range matrix.Roles {
		for _, s := range []string{"a", "b"} {
			c := BehaviorCase{Role: role, Scope: s, Statement: "read", Rows: [][]*string{}}
			if role == reader {
				if s == "a" {
					c.Rows = [][]*string{{&a, nil}}
				} else {
					c.Rows = [][]*string{{&b, &note}}
				}
			}
			matrix.Cases = append(matrix.Cases, c)
			c = BehaviorCase{Role: role, Scope: s, Statement: "write"}
			switch {
			case role == reader:
				c.Denied = tabula.ClassInsufficientPrivilege
			case s == "a":
				c.Affected = &one
			default:
				c.Denied = tabula.ClassRLSDenied
			}
			matrix.Cases = append(matrix.Cases, c)
		}
	}
	failures, err = RunRLSMatrix(ctx, db, matrix)
	require.NoError(t, err)
	require.Empty(t, failures)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+tbl).Scan(&count))
	require.Equal(t, 2, count)
	matrix.Cases[0].Rows = [][]*string{}
	failures, err = RunRLSMatrix(ctx, db, matrix)
	require.NoError(t, err)
	require.Len(t, failures, 1)
}
