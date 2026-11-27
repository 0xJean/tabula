package tabula

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteResultLineageAlternatives(t *testing.T) {
	reg := NewRegistry(RegistryConfig[struct{}]{})
	_, src := statementSource(reg, WriteDefault)
	id, rank := FieldOf(src.Rel(), "id", Int8()), FieldOf(src.Rel(), "rank", Int4())
	value, delta := Input("rank", Int4()), Input("delta", Int8())
	out := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	write := UpdateOf(src).Set(Assign(rank, value.Expr())).AllRows()
	physical := ResultLineage{Relation: `"public"."items"`, Column: "id"}
	for _, tc := range []struct {
		name string
		expr Expr[int64]
		want ResultLineage
	}{
		{"direct", id.Expr(), physical},
		{"cast", Cast(Int8(), id.Expr()), ResultLineage{CastType: `"pg_catalog"."int8"`}},
		{"computed", Plus(id.Expr(), delta.Expr()), ResultLineage{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			DefineReturning(out, tc.name, WithReturning(write, Project(Output(out, "id", Int8()), tc.expr)))
			require.Equal(t, []ResultLineage{tc.want}, out.Definition().StatementShapes[tc.name+".global.write"].ResultLineage)
			field := Export("id", tc.expr)
			derived := Derived("d", Select(field.Project()).From(src))
			name := "derived_" + tc.name
			DefineReturning(out, name, Select(Project(Output(out, "id", Int8()), field.From(derived))).From(derived).
				With(EffectCTE("modified", write)))
			want := ResultLineage{}
			if tc.name == "direct" {
				want = physical
			}
			require.Equal(t, []ResultLineage{want}, out.Definition().StatementShapes[name+".global.write"].ResultLineage)
		})
	}
}

func TestWriteInsertSelectCodecIdentity(t *testing.T) {
	reg := NewRegistry(RegistryConfig[struct{}]{})
	table, _ := statementSource(reg, WriteDefault)
	notes := DefinePlanSource(reg, TableSpec[statementItem, int64]{Name: "public.notes", RelationScope: Global()})
	codec := CatalogType[int32]("pg_catalog", "int4", func(value int32) any { return value })
	target := MapTable(table, ColumnType("rank", codec)).As("i")
	rank := FieldOf(target.Rel(), "rank", codec)
	source := MapTable(notes, ColumnType("rank", codec)).As("n")
	value := FieldOf(source.Rel(), "rank", codec)
	_, err := TryDefineWrite(target, "matching_codec", InsertInto(target).Columns(rank.Column()).FromSelect(
		Select(Export("rank", value.Expr()).Project()).From(source)))
	require.NoError(t, err)
	before := table.Definition()
	other := CatalogType[int32]("pg_catalog", "int4", func(value int32) any { return int64(value) })
	source = MapTable(notes, ColumnType("rank", other)).As("n")
	value = FieldOf(source.Rel(), "rank", other)
	_, err = TryDefineWrite(target, "different_codec", InsertInto(target).Columns(rank.Column()).FromSelect(
		Select(Export("rank", value.Expr()).Project()).From(source)))
	require.ErrorContains(t, err, "incompatible type")
	require.Equal(t, before, table.Definition())
}
