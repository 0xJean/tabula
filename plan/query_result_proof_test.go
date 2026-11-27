package plan

import (
	"github.com/0xJean/tabula"

	"testing"

	"github.com/stretchr/testify/require"
)

func TestQueryDerivedListProof(t *testing.T) {
	_, out, source, id := queryFixture(t)
	value := Export("value", id.Expr())
	d := Derived("d", Select(value.Project()).From(source))
	v := value.From(d)
	query := Select(Project(Output(out, "value", Int8()), v)).From(d).WithList(ReadListSpec{
		Orders: []RowOrder{{Name: "value", Terms: []RowTerm{AscExpr(v)}}}, Pages: RowPages{Keyset: true, Offset: true, MaxLimit: 4, MaxOffset: 8}, ExactCount: true,
	})
	plan, err := TryDefineRead(out, "derived_list", query)
	require.NoError(t, err)
	shape, err := plan.family.variant(false, nil, "list.data.sort.value.after")
	require.NoError(t, err)
	require.Contains(t, shape.compiled.SQL, `) AS "result" WHERE `)
	require.Equal(t, []tabula.KeyRequirement{{Relation: `"public"."items"`, Columns: []string{"id"}}}, shape.shape.Keys)
	require.Contains(t, shape.shape.Comparisons, tabula.ComparisonRequirement{Relation: `"public"."items"`, Column: "id"})
	cursor, err := plan.CursorFrom("value", queryValue{Value: new(int64(2))})
	require.NoError(t, err)
	require.Equal(t, int64(2), cursor.bindings[0].value)
}

func TestQueryExportedWindowProof(t *testing.T) {
	reg, _, source, id := queryFixture(t)
	type result struct {
		ID       int64 `db:"id"`
		Position int64 `db:"position"`
	}
	out := tabula.DefineView(reg, tabula.ViewSpec[result]{Name: "notes"})
	build := func(name string, spec WindowSpec, orderWindow bool) error {
		key := Export("id", id.Expr())
		position := Export("position", Over(RowNumber(), spec))
		d := Derived("ranked", Select(key.Project(), position.Project()).From(source))
		order := []RowTerm{AscExpr(key.From(d))}
		if orderWindow {
			order = append([]RowTerm{AscExpr(position.From(d))}, order...)
		}
		_, err := TryDefineRead(out, name, Select(Project(Output(out, "id", Int8()), key.From(d)), Project(Output(out, "position", Int8()), position.From(d))).From(d).
			WithList(ReadListSpec{Orders: []RowOrder{{Name: "value", Terms: order}}, Pages: RowPages{Keyset: true, MaxLimit: 4}}))
		return err
	}
	require.NoError(t, build("determined", WindowSpec{Order: []RowTerm{Asc(id)}}, true))
	require.ErrorContains(t, build("nondeterministic", WindowSpec{}, true), "proved repeatable")
	require.NoError(t, build("key_only", WindowSpec{}, false))
	shape := out.Definition().StatementShapes["determined.global.list.data.sort.value.after"]
	require.Equal(t, []tabula.KeyRequirement{{Relation: `"public"."items"`, Columns: []string{"id"}}}, shape.Keys)
}

func TestQuerySetKeysAndRepresentatives(t *testing.T) {
	reg, _, source, id := queryFixture(t)
	type row struct {
		Arm bool  `db:"arm"`
		ID  int64 `db:"id"`
	}
	out := tabula.DefineView(reg, tabula.ViewSpec[row]{Name: "notes"})
	firstID, nextID := Export("id", id.Expr()), Export("id", id.Expr())
	first := Export("arm", IsNull(TypedNull(Int8())))
	second := Export("arm", Not(IsNull(TypedNull(Int8()))))
	build := func(name string, firstArm, nextArm Exported[bool]) error {
		left := Select(firstArm.Project(), firstID.Project()).From(source)
		right := Select(nextArm.Project(), nextID.Project()).From(source)
		d := Derived("combined", UnionAll(left, right))
		arm, key := firstArm.From(d), firstID.From(d)
		_, err := TryDefineRead(out, name, Select(Project(Output(out, "arm", Bool()), arm), Project(Output(out, "id", Int8()), key)).From(d).
			WithList(ReadListSpec{Orders: []RowOrder{{Name: "key", Terms: []RowTerm{AscExpr(arm), AscExpr(key)}}}, Pages: RowPages{Keyset: true, MaxLimit: 4}}))
		return err
	}
	require.NoError(t, build("discriminated", first, second))
	require.ErrorContains(t, build("same_arm", first, first), "proved complete result key")
	require.ErrorContains(t, build("bound_arms", Export("arm", Input("a", Bool()).Expr()), Export("arm", Input("b", Bool()).Expr())), "proved complete result key")
	// UNION chooses one equality-class representative. A textual conversion
	// must not turn two numerically equal scales into different page positions.
	numeric := tabula.CatalogType[string]("pg_catalog", "numeric", nil)
	left := Export("value", Input("first", numeric).Expr())
	right := Export("value", Input("second", numeric).Expr())
	d := Derived("numbers", Union(Select(left.Project()), Select(right.Project())))
	type textRow struct {
		Value string `db:"value"`
	}
	text := tabula.DefineView(reg, tabula.ViewSpec[textRow]{Name: "groups"})
	value := Cast(Text(), left.From(d))
	_, err := TryDefineRead(text, "unstable", Select(Project(Output(text, "value", Text()), value)).From(d).
		WithList(ReadListSpec{Orders: []RowOrder{{Name: "value", Terms: []RowTerm{AscExpr(value)}}}, Pages: RowPages{Keyset: true, MaxLimit: 3}}))
	require.Error(t, err)
	_, err = TryDefineRead(text, "native", Select(Project(Output(text, "value", numeric), left.From(d))).From(d).
		WithList(ReadListSpec{Orders: []RowOrder{{Name: "value", Terms: []RowTerm{AscExpr(left.From(d))}}}, Pages: RowPages{Keyset: true, MaxLimit: 3}}))
	require.NoError(t, err)
}

func TestQueryMaterializedVolatilityAndProofClone(t *testing.T) {
	reg, _, source, id := queryFixture(t)
	key := Export("id", id.Expr())
	value := Export("value", GenRandomUUID())
	common := CTE("generated", Select(key.Project(), value.Project()).From(source), Materialized)
	d := common.As("d")
	type row struct {
		ID    int64  `db:"id"`
		Value string `db:"value"`
	}
	out := tabula.DefineView(reg, tabula.ViewSpec[row]{Name: "groups"})
	query := Select(Project(Output(out, "id", Int8()), key.From(d)), Project(Output(out, "value", value.value.kind), value.From(d))).From(d).With(common)
	_, err := TryDefineRead(out, "fixed", query)
	require.NoError(t, err)
	_, err = TryDefineRead(out, "volatile_cursor", query.WithList(ReadListSpec{Orders: []RowOrder{{Name: "key", Terms: []RowTerm{AscExpr(value.From(d)), AscExpr(key.From(d))}}}, Pages: RowPages{Keyset: true, MaxLimit: 2}}))
	require.Error(t, err)
	original := queryResultField{proof: queryValueProof{valid: true, exact: true, keys: []tabula.KeyRequirement{{Relation: "items", Columns: []string{"id"}}}, comparisons: []tabula.ComparisonRequirement{{Relation: "items", Column: "id"}}}}
	cloned := cloneQueryFields([]queryResultField{original})
	cloned[0].proof.keys[0].Columns[0] = "other"
	cloned[0].proof.comparisons[0].Column = "other"
	require.Equal(t, "id", original.proof.keys[0].Columns[0])
	require.Equal(t, "id", original.proof.comparisons[0].Column)
}

func TestQueryUnnestProofAndCountVolatility(t *testing.T) {
	_, out, source, id := queryFixture(t)
	values := Input("values", ArrayOf(Int8()))
	value := UnnestColumn("value", values.Expr(), Int8())
	u := Unnest("u", value)
	spec := ReadListSpec{Orders: []RowOrder{{Name: "value", Terms: []RowTerm{AscExpr(value.From(u))}}}, Pages: RowPages{Keyset: true, MaxLimit: 3}}
	_, err := TryDefineRead(out, "plain", Select(Project(Output(out, "value", Int8()), value.From(u))).From(u).WithList(spec))
	require.ErrorContains(t, err, "complete result key")
	group := Export("value", value.From(u))
	d := Derived("d", Select(group.Project()).From(u).GroupBy(value.From(u)))
	spec.Orders[0].Terms = []RowTerm{AscExpr(group.From(d))}
	spec.ExactCount = true
	_, err = TryDefineRead(out, "grouped", Select(Project(Output(out, "value", Int8()), group.From(d))).From(d).WithList(spec))
	require.NoError(t, err)

	volatile := ScalarSubquery(SelectValue(GenRandomUUID()))
	query := Select(Project(Output(out, "value", Int8()), id.Expr())).From(source).Where(Not(IsNull(volatile))).WithList(ReadListSpec{ExactCount: true})
	_, err = TryDefineRead(out, "count_volatile", query)
	require.ErrorContains(t, err, "volatile")
	_, err = TryDefineRead(out, "count_exists", Select(Project(Output(out, "value", Int8()), id.Expr())).From(source).
		Where(ExistsQuery(Select(Export("id", id.Expr()).Project()).From(source).Where(Not(IsNull(volatile))))).WithList(ReadListSpec{ExactCount: true}))
	require.ErrorContains(t, err, "volatile")
}

func TestQueryWinnerAndOuterArrayProof(t *testing.T) {
	reg, out, source, id := queryFixture(t)
	key := Export("value", id.Expr())
	d := Derived("d", Select(key.Project()).From(source))
	value := key.From(d)
	plan, err := TryDefineRead(out, "winner", Select(Project(Output(out, "value", Int8()), value)).From(d).DistinctOn(value).OrderBy(AscExpr(value)))
	require.NoError(t, err)
	shape, err := plan.family.variant(false, nil, "data")
	require.NoError(t, err)
	require.Equal(t, []tabula.KeyRequirement{{Relation: `"public"."items"`, Columns: []string{"id"}}}, shape.shape.Keys)
	keyless := Derived("keyless", Select(key.Project()).From(source).CrossJoin(d.As("other")))
	_, err = TryDefineRead(out, "unstable_winner", Select(Project(Output(out, "value", Int8()), key.From(keyless))).From(keyless).DistinctOn(key.From(keyless)).OrderBy(AscExpr(key.From(keyless))))
	require.ErrorContains(t, err, "complete result key")
	type row struct {
		Values *string `db:"values"`
	}
	arrays := tabula.DefineView(reg, tabula.ViewSpec[row]{Name: "arrays"})
	_, err = TryDefineRead(arrays, "nullable_elements", Select(Project(Output(arrays, "values", Text()), Cast(Text(), Over(ArrayAgg(value), WindowSpec{})))).
		From(source).LeftJoin(d, Eq(id.Expr(), value)))
	require.ErrorContains(t, err, "array-of-nullable")
}

func TestQueryListPairFixedInputs(t *testing.T) {
	value := Input("value", Int8())
	first, second := newParameterLayout(false), newParameterLayout(false)
	first.graph, second.graph = true, true
	a := &typedInput{name: "@take", kind: typeID(Int8()), fixed: new(int64(2))}
	b := &typedInput{name: "@take", kind: typeID(Int8()), fixed: new(int64(3))}
	_, err := first.parameter(a)
	require.NoError(t, err)
	_, err = first.parameter(value.input)
	require.NoError(t, err)
	_, err = second.parameter(value.input)
	require.NoError(t, err)
	_, err = second.parameter(b)
	require.NoError(t, err)
	shapes := []*compiledShape{{layout: first}, {layout: second}}
	pair, err := readPairBindings([]Binding{BindValue(value, int64(7))}, shapes...)
	require.NoError(t, err)
	for i, want := range [][]any{{int64(2), int64(7)}, {int64(7), int64(3)}} {
		args, err := shapes[i].layout.bind(nil, pair[i])
		require.NoError(t, err)
		require.Equal(t, want, args)
	}
	_, err = readPairBindings(nil, shapes...)
	require.ErrorContains(t, err, "missing binding")
	_, err = readPairBindings([]Binding{BindValue(value, int64(7)), {input: a, value: int64(99)}}, shapes...)
	require.ErrorContains(t, err, "does not belong")
}

func TestQueryLimitedSubsetNeedsTotalOrder(t *testing.T) {
	_, out, source, id := queryFixture(t)
	key := Export("value", id.Expr())
	for name, inner := range map[string]ReadSelect{
		"unordered": Select(key.Project()).From(source).Take(2),
		"ordered":   Select(key.Project()).From(source).OrderBy(Asc(id)).Take(2),
	} {
		d := Derived("d", inner)
		value := key.From(d)
		_, err := TryDefineRead(out, name, Select(Project(Output(out, "value", Int8()), value)).From(d).
			WithList(ReadListSpec{Orders: []RowOrder{{Name: "value", Terms: []RowTerm{AscExpr(value)}}}, Pages: RowPages{Keyset: true, MaxLimit: 2}}))
		if name == "ordered" {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, "proved repeatable")
		}
	}
}

func TestQuerySetCountVolatileEquality(t *testing.T) {
	left := Select(Export("value", GenRandomUUID()).Project())
	right := Select(Export("value", GenRandomUUID()).Project())
	for name, set := range map[string]ReadSelect{
		"union":         Union(left, right),
		"intersect":     Intersect(left, right),
		"intersect_all": IntersectAll(left, right),
		"except":        Except(left, right),
		"except_all":    ExceptAll(left, right),
	} {
		t.Run(name, func(t *testing.T) {
			source := Derived("combined", set)
			query := Select(Export("value", Input("value", Int8()).Expr()).Project()).From(source)
			require.ErrorContains(t, queryCountStability(query, make(map[*commonTableNode]bool)), "volatile")
		})
	}
	// UNION ALL cardinality does not depend on the output equality values.
	require.NoError(t, queryCountStability(UnionAll(left, right), make(map[*commonTableNode]bool)))
}
