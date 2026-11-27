package plan

import (
	"testing"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestTypedColumnLengthAndSecretProjection(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	tab := tabula.Define(reg, tabula.TableSpec[typedItem, int64]{Name: "public.items", RelationScope: tabula.Global()})
	rel := FromTable(tab, "i", tabula.Global())
	type projected struct {
		Label string `db:"label,secret"`
	}
	view := tabula.DefineView(reg, tabula.ViewSpec[projected]{Name: "notes"})
	owner := planView(view)
	c, err := newTypedCompiler(false, rel.source)
	require.NoError(t, err)
	length := Varchar(8)
	slot := Input("label", length)
	fields, result, err := c.project(ownerOutputLayout(owner), []Projection{Project(Output(view, "label", length), slot.Expr())})
	require.NoError(t, err)
	require.True(t, c.hasSecret)
	require.Equal(t, []string{`CAST($1 AS "pg_catalog"."varchar") AS "label"`}, fields)
	read := func(typ tabula.PGType[string]) error { return c.read(FieldOf(rel, "label", typ).ref) }
	require.NoError(t, read(tabula.CatalogType[string]("pg_catalog", "varchar", nil)))
	require.NoError(t, read(length))
	require.NoError(t, read(tabula.CatalogType[string]("pg_catalog", "varchar", nil)))
	require.ErrorContains(t, read(Varchar(12)), "conflicting PostgreSQL types")
	meta, err := c.readRequirements()
	require.NoError(t, err)
	meta.Result = result
	shape, err := c.finishShape("labels", "SELECT "+fields[0]+" FROM public.items", catalogType{}, meta)
	require.NoError(t, err)
	require.True(t, shape.shape.Inputs[0].Secret)
	require.NoError(t, registerShapes(owner, "labels", []*compiledShape{shape}))
	before := view.Definition().StatementShapes["labels"].PhysicalColumns[0]
	require.NotNil(t, before.TypeMod)
	require.EqualValues(t, 12, *before.TypeMod)
	*before.TypeMod = 99
	require.EqualValues(t, 12, *view.Definition().StatementShapes["labels"].PhysicalColumns[0].TypeMod)
	for _, n := range []int{-1, 0, 10_485_761} {
		require.Panics(t, func() { Varchar(n) })
	}
}
