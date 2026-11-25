package sqlcheck

import (
	"strings"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestPolicyClassificationDiagnostics(t *testing.T) {
	id := PolicyID{Relation{"docs", "items"}, "visible"}
	p := Policy{PolicyID: id, Command: "SELECT", Roles: []string{"b", "a"}, Permissive: true, Using: "(scope = 1)"}
	stale := PolicyID{Relation{"docs", "items"}, "retired"}
	rules := []PolicyRule{{Name: "scoped", Roles: []string{"a", "b"}, Using: "scope", Entries: []PolicyID{id, stale}}, {Name: "all", Command: "SELECT"}}
	classes, f, err := ClassifyPolicies([]Policy{p}, rules)
	require.NoError(t, err)
	require.Empty(t, classes)
	require.Len(t, f, 2)
	require.Contains(t, strings.Join(f, " "), "matched 2")
	require.Contains(t, strings.Join(f, " "), "retired")
	exceptions := []PolicyRule{{Name: "registered", Entries: []PolicyID{id}}, {Name: "general", UnregisteredOnly: true, TablePattern: "^items$"}, {Name: "empty", Entries: []PolicyID{}}}
	classes, f, err = ClassifyPolicies([]Policy{p}, exceptions)
	require.NoError(t, err)
	require.Empty(t, f)
	require.Equal(t, "registered", classes[0].Rule)
	rules[0].Using = "other"
	_, f, err = ClassifyPolicies([]Policy{p}, rules[:1])
	require.NoError(t, err)
	require.Len(t, f, 3)
	for _, rule := range []PolicyRule{{}, {Name: "bad", Roles: []string{"r", "r"}}, {Name: "bad", Using: "("}, {Name: "bad", Check: "["}, {Name: "bad", Expression: "("}, {Name: "bad", NotExpression: "["}, {Name: "bad", Command: "NO"}, {Name: "bad", Entries: []PolicyID{id, id}}} {
		_, _, err := ClassifyPolicies(nil, []PolicyRule{rule})
		require.Error(t, err)
	}
	checkOnly := Policy{PolicyID: PolicyID{Relation{"docs", "items"}, "insert_scope"}, Command: "INSERT", Roles: []string{"PUBLIC"}, Check: "(scope = current_setting('example.scope', true))"}
	shapeGuard := Policy{PolicyID: PolicyID{Relation{"docs", "items"}, "shape"}, Command: "ALL", Roles: []string{"PUBLIC"}, Using: "(size >= 0)", Check: "(size >= 0)"}
	classes, f, err = ClassifyPolicies([]Policy{checkOnly, shapeGuard}, []PolicyRule{
		{Name: "scope", Roles: []string{"PUBLIC"}, Expression: `example\.scope`},
		{Name: "shape", Roles: []string{"PUBLIC"}, NotExpression: `example\.scope`},
	})
	require.NoError(t, err)
	require.Empty(t, f)
	require.Equal(t, []PolicyClass{{checkOnly.PolicyID, "scope"}, {shapeGuard.PolicyID, "shape"}}, classes)
}

func TestInferredPolicyAccess(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		cmds []string
	}{
		{"UPDATE docs.items SET id=$1, stamp=pg_catalog.now(), flag=true WHERE true", []string{"UPDATE"}},
		{"UPDATE docs.items SET id=$1::pg_catalog.int4, note=coalesce(note, 'x')", []string{"UPDATE", "SELECT"}},
		{"UPDATE docs.items SET id=extract(epoch FROM pg_catalog.now())::int4 WHERE scope IS NOT DISTINCT FROM $1 RETURNING id", []string{"UPDATE", "SELECT"}},
		{"UPDATE docs.items SET flag=$1::int IS DISTINCT FROM $2::int", []string{"UPDATE"}},
		{"DELETE FROM docs.items WHERE extract(epoch FROM $1::timestamp) > 0", []string{"DELETE"}},
		{"UPDATE docs.items SET note=trim(both ' ' FROM $1::text)", []string{"UPDATE"}},
		{"UPDATE docs.items SET note=trim(trailing ' ' FROM note)", []string{"UPDATE", "SELECT"}},
		{"UPDATE docs.items SET id=extract(epoch FROM stamp)", []string{"UPDATE", "SELECT"}},
		{"UPDATE docs.items SET id=epoch", []string{"UPDATE", "SELECT"}},
		{"SELECT id FROM docs.items WHERE scope IS DISTINCT FROM $1 AND scope IS NOT DISTINCT FROM $2 FOR UPDATE", []string{"SELECT", "UPDATE"}},
		{"DELETE FROM docs.items WHERE true", []string{"DELETE"}},
		{"SELECT 1", nil}, {"VALUES (1)", nil}, {"TABLE docs.items", []string{"SELECT"}},
		{"SELECT note FROM docs.items AS i FOR SHARE", []string{"SELECT", "UPDATE"}},
		{"SELECT note FROM docs.items FOR UPDATE SKIP LOCKED", []string{"SELECT", "UPDATE"}},
		{"SELECT note FROM docs.items AS i FOR NO /* lock */ KEY UPDATE OF i NOWAIT", []string{"SELECT", "UPDATE"}},
		{"TABLE docs.items FOR KEY SHARE", []string{"SELECT", "UPDATE"}},
		{"SELECT substring(note FOR 3) FROM docs.items", []string{"SELECT"}},
		{"SELECT substring(note FROM 1 FOR 3) FROM docs.items", []string{"SELECT"}},
		{"SELECT id FROM docs.items WHERE substring(note FOR 3) = 'FOR SHARE'", []string{"SELECT"}},
		{"SELECT note FROM docs.items ORDER BY substring(note FOR 3) FOR SHARE", []string{"SELECT", "UPDATE"}},
		{`SELECT note AS "for" FROM docs.items ORDER BY "for"`, []string{"SELECT"}},
		{"INSERT INTO docs.items DEFAULT VALUES", []string{"INSERT"}},
		{"INSERT INTO docs.items VALUES(1) ON CONFLICT DO NOTHING", []string{"INSERT"}},
		{"INSERT INTO docs.items VALUES(1) ON CONFLICT(id) DO NOTHING", []string{"INSERT", "SELECT"}},
		{"INSERT INTO docs.items VALUES(1) RETURNING id", []string{"INSERT", "SELECT"}},
		{"UPDATE ONLY docs.items SET id=1", []string{"UPDATE"}}, {"UPDATE docs.items SET id=id+1", []string{"UPDATE", "SELECT"}},
		{"DELETE FROM docs.items", []string{"DELETE"}}, {"DELETE FROM docs.items WHERE id=$1", []string{"DELETE", "SELECT"}},
	} {
		got, err := inferAccess(tc.sql)
		require.NoError(t, err, tc.sql)
		var cmds []string
		for _, a := range got {
			require.Equal(t, `"docs"."items"`, a.Table)
			cmds = append(cmds, a.Command)
		}
		require.ElementsMatch(t, tc.cmds, cmds, tc.sql)
	}
	for _, q := range []string{"", "WITH q AS (SELECT 1) SELECT * FROM q", "SELECT id FROM a JOIN b ON a.id=b.id", "SELECT * FROM a,b", "SELECT * FROM f()", "SELECT (TABLE other)", "SELECT (SELECT id FROM a FOR SHARE) FROM b", "DELETE FROM a USING b", "UPDATE a SET x=y FROM b"} {
		_, err := inferAccess(q)
		require.Error(t, err, q)
	}
	got, err := inferAccess(`SELECT ';' FROM "Case"."With.dot"`)
	require.NoError(t, err)
	require.Equal(t, `"Case"."With.dot"`, got[0].Table)
}

func TestACLTypesAndOptions(t *testing.T) {
	scope := CatalogScope{[]string{"s"}, []string{"public reader"}}
	g := Grant{Role: scope.Roles[0], Schema: "s", Kind: "FUNCTION", Object: `f"n`, Privilege: "EXECUTE", Arguments: `pg_catalog.int4, "Weird S"."With.dot"`}
	q, err := g.sql(true, false)
	require.NoError(t, err)
	require.Equal(t, `GRANT EXECUTE ON FUNCTION "s"."f""n"("pg_catalog"."int4", "Weird S"."With.dot") TO "public reader";`, q)
	high := g
	high.GrantOption = true
	d, err := CompareGrants(GrantManifest{scope, []Grant{g}}, []Grant{high})
	require.NoError(t, err)
	require.Len(t, d.SQL, 1)
	require.Contains(t, d.SQL[0], "REVOKE GRANT OPTION FOR")
	d, err = CompareGrants(GrantManifest{scope, []Grant{high}}, []Grant{g})
	require.NoError(t, err)
	require.Len(t, d.SQL, 1)
	require.Contains(t, d.SQL[0], "WITH GRANT OPTION")
	_, err = CompareGrants(GrantManifest{scope, []Grant{g, g}}, nil)
	require.Error(t, err)
	for _, bad := range []string{"int4", "pg_catalog.int4,", "pg_catalog.int4 junk", "pg_catalog.int4[]", "pg_catalog.int4);"} {
		_, err := signature(bad)
		require.Error(t, err)
	}
	_, err = CompareGrants(GrantManifest{CatalogScope{[]string{"other"}, scope.Roles}, nil}, []Grant{g})
	require.Error(t, err)
	g.Kind = "COLUMN"
	_, err = g.sql(false, false)
	require.Error(t, err)
}

func TestMatrixRejectsAmbiguousExpectations(t *testing.T) {
	m := RLSMatrix{Roles: []string{"r"}, Scopes: map[string]map[string]string{"a": {}}, Statements: map[string]BehaviorStatement{"one": {SQL: "SELECT 1"}}}
	require.ErrorContains(t, m.validate(), "incomplete")
	c := BehaviorCase{Role: "r", Scope: "a", Statement: "one"}
	m.Cases = []BehaviorCase{c}
	require.ErrorContains(t, m.validate(), "one expectation")
	c.Rows = [][]*string{}
	m.Cases = []BehaviorCase{c}
	require.NoError(t, m.validate())
	m.Cases = append(m.Cases, c)
	require.ErrorContains(t, m.validate(), "duplicate")
	c.Denied = tabula.ClassRLSDenied
	m.Cases = []BehaviorCase{c}
	require.ErrorContains(t, m.validate(), "one expectation")
	c.Rows = nil
	c.Denied = tabula.ClassUnique
	m.Cases = []BehaviorCase{c}
	require.ErrorContains(t, m.validate(), "unknown denial")
	m.Scopes["a"] = map[string]string{"role": "x"}
	require.ErrorContains(t, m.validate(), "custom GUC")
	for _, work := range []RoleStatements{nil, {"": {{Name: "one", SQL: "SELECT 1"}}}, {"r": {}}, {"r": {{Name: "one", SQL: "SELECT 1; SELECT 2"}}}, {"r": {{Name: "one", SQL: "SELECT 1"}, {Name: "one", SQL: "SELECT 2"}}}, {"r": {{Name: "one", SQL: "SELECT * FROM f()", NoAccess: true, Access: []PolicyAccess{{"docs.items", "SELECT"}}}}}} {
		require.Error(t, work.validate())
	}
	inv := SQLInventory{Sites: []SQLSite{{ID: "fn", SQL: "SELECT * FROM f()"}}}
	bound, err := inv.BindRoles(map[string]SQLBinding{"fn": {Roles: []string{"r"}, NoAccess: true}})
	require.NoError(t, err)
	require.True(t, bound["r"][0].NoAccess)
	_, err = inv.BindRoles(map[string]SQLBinding{"fn": {Roles: []string{"r"}, NoAccess: true, Access: []PolicyAccess{{"docs.items", "SELECT"}}}})
	require.ErrorContains(t, err, "NoAccess")
}
