//go:build integration

package sqlcheck

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

type countedCatalog struct {
	queryer
	calls int
}

func (c *countedCatalog) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	c.calls++
	return c.queryer.QueryContext(ctx, q, args...)
}

func TestReadinessPostgres(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	schema := fmt.Sprintf("tabula_ready_%d", time.Now().UnixNano())
	role, member, auditor := schema+"_role", schema+"_member", schema+"_audit"
	items, seq, fn := schema+".items", schema+".counter", schema+".lookup"
	mustExec := func(q string) {
		t.Helper()
		_, err := db.ExecContext(ctx, q)
		require.NoError(t, err, q)
	}
	for _, name := range []string{role, member, auditor} {
		mustExec("CREATE ROLE " + name)
		t.Cleanup(func() { _, _ = db.Exec("DROP ROLE " + name) })
	}
	mustExec("CREATE SCHEMA " + schema)
	t.Cleanup(func() { _, _ = db.Exec("DROP SCHEMA " + schema + " CASCADE") })
	for _, q := range []string{
		"GRANT " + member + " TO " + role,
		"GRANT USAGE ON SCHEMA " + schema + " TO " + member,
		"CREATE TABLE " + items + " (id bigint GENERATED ALWAYS AS IDENTITY, value text, extra int)",
		"ALTER TABLE " + items + " ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE " + items + " FORCE ROW LEVEL SECURITY",
		"GRANT SELECT(id), INSERT(value), UPDATE(value), DELETE ON " + items + " TO " + member,
		"CREATE SEQUENCE " + seq,
		"GRANT USAGE ON ALL SEQUENCES IN SCHEMA " + schema + " TO " + member,
		"CREATE DOMAIN " + schema + ".kind AS text",
		"CREATE FUNCTION " + fn + "(integer, " + schema + ".kind[]) RETURNS integer LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION " + fn + "(text) RETURNS integer LANGUAGE sql AS 'SELECT 1'",
		"REVOKE EXECUTE ON FUNCTION " + fn + "(text) FROM PUBLIC",
	} {
		mustExec(q)
	}
	// A PUBLIC overload, inherited grants and an identity-owned sequence.
	sig := fn + "(integer, " + schema + ".kind[])"
	privs, _ := CollectPrivilegeNeeds([]tabula.Definition{{Name: items, Privileges: []tabula.DefinedPrivilege{
		{Table: items, Privilege: "SELECT", Columns: []string{"id"}},
		{Table: items, Privilege: "INSERT", Columns: []string{"value"}},
		{Table: items, Privilege: "UPDATE", Columns: []string{"value"}},
		{Table: items, Privilege: "DELETE"},
		{Table: items, Privilege: "USAGE", Sequence: "id"},
		{Function: sig, Privilege: "EXECUTE"},
	}}})
	derived := ReadinessNeeds{Privileges: privs}
	manual := ReadinessNeeds{SchemaUsage: []string{schema}, AnyColumnSelect: []string{items}, ForceRLS: []string{items}, SequenceUsage: []string{seq}}
	found, err := CheckReadiness(ctx, db, role, derived, manual)
	require.NoError(t, err)
	require.Empty(t, found)

	// The connection checking readiness has no privileges on the target schema.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, "SET LOCAL ROLE "+auditor)
	require.NoError(t, err)
	var canUse bool
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT has_schema_privilege(current_user,$1,'USAGE')", schema).Scan(&canUse))
	require.False(t, canUse)
	counted := &countedCatalog{queryer: tx}
	check := func(target string, groups ...ReadinessNeeds) []MissingRequirement {
		t.Helper()
		before := counted.calls
		got, err := checkReadiness(ctx, counted, target, groups)
		require.NoError(t, err)
		require.Equal(t, before+1, counted.calls)
		return got
	}
	require.Empty(t, check(role, manual, derived, manual))
	require.Equal(t, []MissingRequirement{{"ROLE", role + "_missing", "", "EXISTS"}}, check(role+"_missing", manual))
	absent := schema + ".absent"
	require.Equal(t, []MissingRequirement{
		{"COLUMN", items, "extra", "SELECT"},
		{"COLUMN", items, "missing", "UPDATE"},
		{"FUNCTION", fn + "(" + schema + ".missing_type)", "", "EXECUTE"},
		{"FUNCTION", fn + "(text)", "", "EXECUTE"},
		{"FUNCTION", schema + ".unknown()", "", "EXECUTE"},
		{"SCHEMA", schema + "_missing", "", "USAGE"},
		{"SEQUENCE", absent, "", "USAGE"},
		{"SEQUENCE", items, "extra", "USAGE"},
		{"TABLE", absent, "", "DELETE"},
		{"TABLE", absent, "", "ENABLE RLS"},
		{"TABLE", absent, "", "FORCE RLS"},
		{"TABLE", absent, "", "SELECT ANY COLUMN"},
	}, check(role, ReadinessNeeds{
		Privileges: PrivilegeNeeds{
			Columns:     map[string]map[string]map[string]bool{items: {"SELECT": {"extra": true}, "UPDATE": {"missing": true}}},
			TableDelete: map[string]bool{absent: true}, Sequences: [][2]string{{items, "extra"}},
			Functions: []string{fn + "(text)", schema + ".unknown()", fn + "(" + schema + ".missing_type)"},
		}, SchemaUsage: []string{schema + "_missing"}, AnyColumnSelect: []string{absent},
		ForceRLS: []string{absent}, SequenceUsage: []string{absent},
	}))
	cols := map[string]bool{}
	for i := range 256 {
		cols[fmt.Sprintf("absent_%03d", i)] = true
	}
	many := ReadinessNeeds{Privileges: PrivilegeNeeds{Columns: map[string]map[string]map[string]bool{items: {"SELECT": cols}}}}
	require.Len(t, check(role, many, many), len(cols))
	require.Equal(t, []MissingRequirement{{"TABLE", items, "", "SELECT ANY COLUMN"}}, check(auditor, ReadinessNeeds{AnyColumnSelect: []string{items}, ForceRLS: []string{items}}))
	require.NoError(t, tx.Rollback())
	for _, clause := range []string{"NO FORCE ROW LEVEL SECURITY", "DISABLE ROW LEVEL SECURITY"} {
		mustExec("ALTER TABLE " + items + " " + clause)
		found, err = CheckReadiness(ctx, db, role, manual)
		require.NoError(t, err)
		if clause == "NO FORCE ROW LEVEL SECURITY" {
			require.Equal(t, []MissingRequirement{{"TABLE", items, "", "FORCE RLS"}}, found)
		} else {
			require.Equal(t, []MissingRequirement{{"TABLE", items, "", "ENABLE RLS"}, {"TABLE", items, "", "FORCE RLS"}}, found)
		}
	}
}
