package sqlcheck

import (
	"testing"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestReadinessNames(t *testing.T) {
	item, err := resolveReadinessItem(MissingRequirement{Kind: "FUNCTION", Object: `"A.B"."Fn"(numeric(10,2), timestamp with time zone, "Type Schema"."A""B"[][])`})
	require.NoError(t, err)
	require.Equal(t, "A.B", item.Schema)
	require.Equal(t, "Fn", item.Name)
	require.Equal(t, []readinessType{{Spelling: "numeric(10,2)"}, {Spelling: "timestamp with time zone"}, {Spelling: `"Type Schema"."A""B"[][]`, Schema: "Type Schema", Name: `A"B`, Array: true}}, item.Args)
	for _, sig := range []string{"fn()", "s.f", "s.f(,text)", "s.f(text,)", "s.f(()),other()", "s.f($1)", "s.f('text')", "s.f(s.t text)"} {
		_, err := readinessItems(ReadinessNeeds{Privileges: PrivilegeNeeds{Functions: []string{sig}}})
		require.Error(t, err, sig)
	}
	_, err = readinessItems(ReadinessNeeds{Privileges: PrivilegeNeeds{Columns: map[string]map[string]map[string]bool{"s.t": {"READ": {"a": true}}}}})
	require.Error(t, err)
	_, err = CheckReadiness(t.Context(), nil, "r")
	require.Error(t, err)
}

func TestReadinessRoutineNames(t *testing.T) {
	for _, r := range []tabula.RoutineRequirement{
		{Signature: "lookup()", ResultType: "pg_catalog.int8"},
		{Signature: "public.lookup", ResultType: "pg_catalog.int8"},
		{Signature: "public.lookup()", ResultType: "bigint"},
		{Signature: "public.lookup()", ResultType: "pg_catalog.int8()"},
	} {
		_, err := readinessItems(ReadinessNeeds{Routines: []tabula.RoutineRequirement{r}})
		require.Error(t, err)
	}
}
