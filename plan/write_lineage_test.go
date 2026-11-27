package plan

import (
	"github.com/0xJean/tabula"

	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteResultLineageAlternatives(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	_, src := statementSource(reg, tabula.WriteDefault)
	id, rank := FieldOf(src.Rel(), "id", Int8()), FieldOf(src.Rel(), "rank", Int4())
	value, delta := Input("rank", Int4()), Input("delta", Int8())
	out := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "entries"})
	write := UpdateOf(src).Set(Assign(rank, value.Expr())).AllRows()
	physical := tabula.ResultLineage{Relation: `"public"."items"`, Column: "id"}
	for _, tc := range []struct {
		name string
		expr Expr[int64]
		want tabula.ResultLineage
	}{
		{"direct", id.Expr(), physical},
		{"cast", Cast(Int8(), id.Expr()), tabula.ResultLineage{CastType: `"pg_catalog"."int8"`}},
		{"computed", Plus(id.Expr(), delta.Expr()), tabula.ResultLineage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			DefineReturning(out, tc.name, WithReturning(write, Project(Output(out, "id", Int8()), tc.expr)))
			require.Equal(t, []tabula.ResultLineage{tc.want}, out.Definition().StatementShapes[tc.name+".global.write"].ResultLineage)
			field := Export("id", tc.expr)
			derived := Derived("d", Select(field.Project()).From(src))
			name := "derived_" + tc.name
			DefineReturning(out, name, Select(Project(Output(out, "id", Int8()), field.From(derived))).From(derived).
				With(EffectCTE("modified", write)))
			want := tabula.ResultLineage{}
			if tc.name == "direct" {
				want = physical
			}
			require.Equal(t, []tabula.ResultLineage{want}, out.Definition().StatementShapes[name+".global.write"].ResultLineage)
		})
	}
}

func TestWriteInsertSelectCodecIdentity(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table, _ := statementSource(reg, tabula.WriteDefault)
	notes := tabula.DefinePlanSource(reg, tabula.TableSpec[statementItem, int64]{Name: "public.notes", RelationScope: tabula.Global()})
	codec := tabula.CatalogType[int32]("pg_catalog", "int4", func(value int32) any { return value })
	target := MapTable(table, ColumnType("rank", codec)).As("i")
	rank := FieldOf(target.Rel(), "rank", codec)
	source := MapTable(notes, ColumnType("rank", codec)).As("n")
	value := FieldOf(source.Rel(), "rank", codec)
	_, err := TryDefineWrite(target, "matching_codec", InsertInto(target).Columns(rank.Column()).FromSelect(
		Select(Export("rank", value.Expr()).Project()).From(source)))
	require.NoError(t, err)
	before := table.Definition()
	other := tabula.CatalogType[int32]("pg_catalog", "int4", func(value int32) any { return int64(value) })
	source = MapTable(notes, ColumnType("rank", other)).As("n")
	value = FieldOf(source.Rel(), "rank", other)
	_, err = TryDefineWrite(target, "different_codec", InsertInto(target).Columns(rank.Column()).FromSelect(
		Select(Export("rank", value.Expr()).Project()).From(source)))
	require.ErrorContains(t, err, "incompatible type")
	require.Equal(t, before, table.Definition())
}
