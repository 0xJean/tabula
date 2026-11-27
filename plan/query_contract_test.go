package plan

import (
	"github.com/0xJean/tabula"

	"testing"

	"github.com/stretchr/testify/require"
)

func TestQueryContractRejectionsAreAtomic(t *testing.T) {
	_, out, source, id := queryFixture(t)
	value := Export("value", id.Expr())
	input := Input("value", Int8())
	first := CTE("first", Select(value.Project()).From(source), DefaultMaterialization)
	f := first.As("f")
	forward := CTE("second", Select(value.Project()).From(f), DefaultMaterialization)
	second := Export("value", value.From(f))
	dependent := CTE("second", Select(second.Project()).From(f), DefaultMaterialization)
	d := dependent.As("d")
	one := Export("value", input.Expr())
	two := Export("other", input.Expr())
	badArity := Derived("d", UnionAll(Select(one.Project()), Select(one.Project(), two.Project())))
	wrong := Export("value", Input("text", Text()).Expr())
	badType := Derived("d", UnionAll(Select(one.Project()), Select(wrong.Project())))
	array := UnnestColumn("value", ArrayExpr(id.Expr()), Int8())
	u := Unnest("u", array)
	badScalar := SelectValue(input.Expr())
	badScalar.query.project = []Projection{one.Project(), two.Project()}
	queries := []ReadSelect{
		Select(Project(Output(out, "value", Int8()), id.Expr())).CrossJoin(source),
		Select(Project(Output(out, "value", Int8()), id.Expr())).From(source).CrossJoin(source),
		Select(Project(Output(out, "value", Int8()), value.From(f))).From(f).With(forward, first),
		Select(Project(Output(out, "value", Int8()), second.From(d))).From(d).With(dependent, first),
		Select(Project(Output(out, "value", Int8()), one.From(badArity))).From(badArity),
		Select(Project(Output(out, "value", Int8()), one.From(badType))).From(badType),
		Select(Project(Output(out, "value", Int8()), array.From(u))).From(source).CrossLateral(u),
		Select(Project(Output(out, "value", Int8()), ScalarSubquery(badScalar))),
	}
	before := out.Definition()
	for _, query := range queries {
		_, err := TryDefineRead(out, "invalid", query)
		require.Error(t, err)
		require.Equal(t, before, out.Definition())
	}
}

func TestQueryRecursiveResultCannotLaunderProvenance(t *testing.T) {
	reg, out, source, id := queryFixture(t)
	other := tabula.DefinePlanSource(reg, tabula.TableSpec[queryItem, int64]{Name: "public.notes", RelationScope: tabula.Global()})
	n := MapTable(other).As("n")
	field := FieldOf(n.Rel(), "id", Int8())
	value := Export("value", id.Expr())
	common := RecursiveCTE("groups", Select(value.Project()).From(source), RecursiveUnionAll, func(self QuerySource) ReadSelect {
		return Select(Export("value", field.Expr()).Project()).From(self).InnerJoin(n, Eq(field.Expr(), value.From(self)))
	})
	r := common.As("r")
	plan, err := TryDefineRead(out, "recursive_origins", Select(Project(Output(out, "value", Int8()), value.From(r))).From(r).With(common))
	require.NoError(t, err)
	shape, err := plan.family.variant(false, nil, "data")
	require.NoError(t, err)
	require.Equal(t, []tabula.ResultLineage{{}}, shape.shape.ResultLineage)
}

func TestQueryArrayCodecIdentity(t *testing.T) {
	_, out, _, _ := queryFixture(t)
	plain := Input("values", ArrayOf(Int8()))
	custom := Input("values", ArrayOf(Int8().WithEncoder(func(n int64) (any, error) { return n, nil })))
	a, b := Export("value", plain.Expr()), Export("value", custom.Expr())
	source := Derived("d", UnionAll(Select(a.Project()), Select(b.Project())))
	_, err := TryDefineRead(out, "array_codec", Select(Project(Output(out, "value", Int8()), CountAll())).From(source))
	require.ErrorContains(t, err, "codecs")
}

func TestQueryOuterArrayAggregateNeedsNullableElements(t *testing.T) {
	reg, _, source, id := queryFixture(t)
	type result struct {
		Value *string `db:"value"`
	}
	out := tabula.DefineView(reg, tabula.ViewSpec[result]{Name: "notes"})
	value := Export("value", id.Expr())
	d := Derived("d", Select(value.Project()).From(source))
	on := IsNotDistinct(id.Expr(), id.Expr())
	bad := Cast(Text(), ArrayAgg(value.From(d)))
	_, err := TryDefineRead(out, "plain", Select(Project(Output(out, "value", Text()), bad)).From(source).LeftJoin(d, on))
	require.ErrorContains(t, err, "array-of-nullable")
	good := Cast(Text(), ArrayAggAs(ArrayOfNullable(Int8()), value.From(d)))
	_, err = TryDefineRead(out, "nullable", Select(Project(Output(out, "value", Text()), good)).From(source).LeftJoin(d, on))
	require.NoError(t, err)
}

func TestQueryDirectLineageIsSeparateFromDependencies(t *testing.T) {
	_, out, source, id := queryFixture(t)
	input := Input("one", Int8())
	physical := tabula.ResultLineage{Relation: source.rel.table.SQL(), Column: "id"}
	for _, test := range []struct {
		name  string
		value Expr[int64]
		want  tabula.ResultLineage
	}{
		{"direct", id.Expr(), physical},
		{"cast", Cast(Int8(), id.Expr()), tabula.ResultLineage{CastType: typeID(Int8()).Qualified()}},
		{"computed", Plus(id.Expr(), input.Expr()), tabula.ResultLineage{}},
	} {
		plan, err := TryDefineRead(out, test.name, Select(Project(Output(out, "value", Int8()), test.value)).From(source).Take(1))
		require.NoError(t, err)
		shape, err := plan.family.variant(false, nil, "data")
		require.NoError(t, err)
		require.Equal(t, []tabula.ResultLineage{test.want}, shape.shape.ResultLineage)
		value := Export("value", test.value)
		d := Derived("d", Select(value.Project()).From(source))
		plan, err = TryDefineRead(out, test.name+"_export", Select(Project(Output(out, "value", Int8()), value.From(d))).From(d))
		require.NoError(t, err)
		shape, err = plan.family.variant(false, nil, "data")
		require.NoError(t, err)
		want := test.want
		if want.CastType != "" {
			want = tabula.ResultLineage{}
		}
		require.Equal(t, []tabula.ResultLineage{want}, shape.shape.ResultLineage)
	}
}
