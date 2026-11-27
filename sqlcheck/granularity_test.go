package sqlcheck

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseGrantGranularity(t *testing.T) {
	g, err := ParseGrantGranularity("")
	require.NoError(t, err)
	require.Equal(t, GranularityAll, g)

	g, err = ParseGrantGranularity("all")
	require.NoError(t, err)
	require.Equal(t, GranularityAll, g)

	g, err = ParseGrantGranularity("TABLE")
	require.NoError(t, err)
	require.Equal(t, GranularityTable, g)

	_, err = ParseGrantGranularity("column")
	require.Error(t, err)
}

func TestCompareGrantsGranularityTable(t *testing.T) {
	scope := CatalogScope{
		Schemas:     []string{"app"},
		Roles:       []string{"reader"},
		Granularity: GranularityTable,
	}
	tblSelect := Grant{Role: "reader", Schema: "app", Kind: "TABLE", Object: "items", Privilege: "SELECT"}
	tblUpdate := Grant{Role: "reader", Schema: "app", Kind: "TABLE", Object: "items", Privilege: "UPDATE"}
	colSelect := Grant{Role: "reader", Schema: "app", Kind: "COLUMN", Object: "items", Column: "id", Privilege: "SELECT"}

	diff, err := CompareGrants(GrantManifest{CatalogScope: scope, Grants: []Grant{tblSelect}}, []Grant{tblSelect, tblUpdate})
	require.NoError(t, err)
	require.Empty(t, diff.Missing)
	require.Equal(t, []Grant{tblUpdate}, diff.Extra)
	require.Equal(t, []string{`REVOKE UPDATE ON TABLE "app"."items" FROM "reader" RESTRICT;`}, diff.SQL)

	// Column grants in manifest are rejected under GranularityTable.
	_, err = CompareGrants(GrantManifest{CatalogScope: scope, Grants: []Grant{colSelect}}, []Grant{tblSelect})
	require.ErrorContains(t, err, "GranularityTable")

	// Column grants in actual are also rejected under GranularityTable.
	_, err = CompareGrants(GrantManifest{CatalogScope: scope, Grants: []Grant{tblSelect}}, []Grant{tblSelect, colSelect})
	require.ErrorContains(t, err, "GranularityTable")

	// Invalid granularity rejected.
	badScope := scope
	badScope.Granularity = GrantGranularity("invalid")
	_, err = CompareGrants(GrantManifest{CatalogScope: badScope}, nil)
	require.ErrorContains(t, err, "invalid grant granularity")
}

func TestReadinessItemsTablePrivileges(t *testing.T) {
	items, err := readinessItems(ReadinessNeeds{
		TableSelect: []string{"public.items"},
		TableInsert: []string{"public.items"},
		TableUpdate: []string{"public.items"},
		TableDelete: []string{"public.items"},
	})
	require.NoError(t, err)
	require.Len(t, items, 4)
	var privs []string
	for _, it := range items {
		require.Equal(t, "TABLE", it.Kind)
		require.Equal(t, "public.items", it.Object)
		require.Empty(t, it.Column)
		privs = append(privs, it.Privilege)
	}
	require.Equal(t, []string{"SELECT", "INSERT", "UPDATE", "DELETE"}, privs)

	_, err = readinessItems(ReadinessNeeds{TableSelect: []string{"unqualified"}})
	require.ErrorContains(t, err, "schema-qualified")
}

func TestStripSQLRowLock(t *testing.T) {
	base := `SELECT id, name FROM public.items WHERE id = $1`
	for _, suffix := range []string{
		" FOR UPDATE",
		" FOR NO KEY UPDATE",
		" FOR SHARE",
		" FOR KEY SHARE",
		" FOR UPDATE NOWAIT",
		" FOR SHARE SKIP LOCKED",
		" FOR UPDATE OF items",
		` FOR UPDATE OF "items" FOR SHARE OF "notes" SKIP LOCKED`,
	} {
		got, err := stripSQLRowLock(base + suffix)
		require.NoError(t, err, suffix)
		require.Equal(t, base, got, suffix)
	}

	_, err := stripSQLRowLock(`SELECT id FROM (SELECT id FROM public.items FOR UPDATE) sub`)
	require.ErrorIs(t, err, ErrDescribeRequired)
}
