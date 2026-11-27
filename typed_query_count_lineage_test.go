package tabula

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQueryCountHasItsOwnResultLineage(t *testing.T) {
	reg, _, source, id := queryFixture(t)
	type row struct {
		ID    int64 `db:"id"`
		Value int64 `db:"value"`
	}
	out := DefineView(reg, ViewSpec[row]{Name: "notes"})
	key, value := Export("id", id.Expr()), Export("value", id.Expr())
	derived := Derived("d", Select(key.Project(), value.Project()).From(source))
	DefineRead(out, "listed", Select(
		Project(Output(out, "id", Int8()), key.From(derived)),
		Project(Output(out, "value", Int8()), value.From(derived)),
	).From(derived).WithList(ReadListSpec{ExactCount: true}))
	def := out.Definition()
	data := def.StatementShapes["listed.global.data"]
	require.Len(t, data.Result, 2)
	require.Len(t, data.ResultLineage, 2)
	count, ok := def.StatementShapes["listed.global.list.count"]
	require.True(t, ok)
	require.Equal(t, []ShapeField{{Name: "count", Type: Int8().id.qualified()}}, count.Result)
	require.Empty(t, count.ResultLineage)
	require.Contains(t, count.Privileges, DefinedPrivilege{Function: "pg_catalog.count()", Privilege: "EXECUTE"})
}
