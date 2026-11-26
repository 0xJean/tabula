package tabula

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScalarMutationValidation(t *testing.T) {
	type item struct {
		ID     int64  `db:"id,key"`
		Source *int32 `db:"source,secret"`
		Target int32  `db:"target"`
	}
	reg := NewRegistry(RegistryConfig[struct{}]{})
	table := DefinePlanSource(reg, TableSpec[item, int64]{Name: "public.items", RelationScope: Global(), WriteMode: WriteUpdateOnly, UpdateColumns: []string{"target"}})
	layout := MapTable(table)
	src := layout.As("i")
	foreign := layout.As("other")
	id := MappedColumn[int64](layout, "id").On(src.Rel())
	read := MappedColumn[int32](layout, "source").On(src.Rel())
	target := MappedColumn[int32](layout, "target").On(src.Rel())
	input, which := Input("value", Int4()), Input("id", Int8())
	filter := MutationSpec{RowFilter: RowFilter{Where: Eq(id.Expr(), which.Expr())}}
	for _, assignment := range []RowAssignment{
		Assign(target, NullIf(input.Expr(), input.Expr())),
		Assign(target, Coalesce(MappedColumn[int32](layout, "source").On(foreign.Rel()).Expr(), input.Expr())),
		Assign(target, Coalesce(MinOf(input.Expr()), input.Expr())),
		Assign(target, DivideAs(Int4(), Input("text", Text()).Expr(), input.Expr())),
		Assign(id, which.Expr()),
	} {
		_, err := TryDefineUpdateMany(src, "invalid", filter, RowSet{Name: "value", Assignments: []RowAssignment{assignment}})
		require.Error(t, err)
		require.Empty(t, table.Definition().Statements)
	}
	_, err := TryDefineUpdateMany(src, "valid", filter, RowSet{Name: "value", Assignments: []RowAssignment{
		Assign(target, Case(When(IsNull(read.Expr()), input.Expr())).Else(Coalesce(read.Expr(), input.Expr()))),
	}})
	require.NoError(t, err)
	for _, shape := range table.Definition().StatementShapes {
		var reads, writes []string
		for _, grant := range shape.Privileges {
			if grant.Privilege == "SELECT" {
				reads = grant.Columns
			}
			if grant.Privilege == "UPDATE" {
				writes = grant.Columns
			}
		}
		require.Equal(t, []string{"id", "source"}, reads)
		require.Equal(t, []string{"target"}, writes)
		for _, input := range shape.Inputs {
			require.True(t, input.Secret)
		}
	}
}
