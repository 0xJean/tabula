package tabula

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type queryItem struct {
	ID    int64  `db:"id,key"`
	Label string `db:"label"`
}
type queryValue struct {
	Value *int64 `db:"value"`
}

func queryFixture(t *testing.T) (*Registry[struct{}], *View[queryValue, struct{}], RowSource[queryItem, struct{}], Field[int64]) {
	t.Helper()
	r := NewRegistry(RegistryConfig[struct{}]{})
	table := DefinePlanSource(r, TableSpec[queryItem, int64]{Name: "public.items", RelationScope: Global()})
	layout := MapTable(table)
	source := layout.As("i")
	id := MappedColumn[int64](layout, "id").On(source.Rel())
	out := DefineView(r, ViewSpec[queryValue]{Name: "entries"})
	return r, out, source, id
}
func TestQuerySourceFreeAndIdentity(t *testing.T) {
	_, out, _, _ := queryFixture(t)
	one, two := Input("value", Int8()), Input("value", Int8())
	nested := ScalarSubquery(SelectValue(two.Expr()).Take(1))
	q := Select(Project(Output(out, "value", Int8()), Plus(one.Expr(), nested)))
	plan, err := TryDefineRead(out, "values", q)
	require.NoError(t, err)
	shape, err := plan.family.variant(false, nil, "data")
	require.NoError(t, err)
	require.Empty(t, shape.shape.Access)
	require.Equal(t, EffectRead, shape.shape.Effects)
	args, err := shape.layout.bind(nil, []Binding{BindValue(one, int64(2)), BindValue(two, int64(3))})
	require.NoError(t, err)
	require.Equal(t, []any{int64(2), int64(3), int64(1)}, args)
	names := make(map[string]bool)
	for _, in := range shape.shape.Inputs {
		require.False(t, names[in.Name])
		names[in.Name] = true
	}
	flat := newParameterLayout(false)
	_, err = flat.parameter(one.input)
	require.NoError(t, err)
	_, err = flat.parameter(two.input)
	require.Error(t, err)
	reused := Select(Project(Output(out, "value", Int8()), Plus(one.Expr(), ScalarSubquery(SelectValue(one.Expr())))))
	p, err := TryDefineRead(out, "reused", reused)
	require.NoError(t, err)
	s, _ := p.family.variant(false, nil, "data")
	require.Len(t, s.layout.order, 1)
}
func TestQueryExportsAndLexicalScope(t *testing.T) {
	reg, out, source, id := queryFixture(t)
	value := Export("value", id.Expr())
	d := Derived("d", Select(value.Project()).From(source))
	q := Select(Project(Output(out, "value", Int8()), value.From(d))).From(d)
	plan, err := TryDefineRead(out, "derived", q)
	require.NoError(t, err)
	s, _ := plan.family.variant(false, nil, "data")
	require.Contains(t, s.sql, `FROM (SELECT`)
	require.Len(t, s.shape.Access, 1)
	before := out.Definition().Statements
	wrong := Export("value", id.Expr())
	_, err = TryDefineRead(out, "wrong", Select(Project(Output(out, "value", Int8()), wrong.From(d))).From(d))
	require.Error(t, err)
	require.Equal(t, before, out.Definition().Statements)
	otherTable := DefinePlanSource(reg, TableSpec[queryItem, int64]{Name: "public.notes", RelationScope: Global()})
	inner := MapTable(otherTable).As("i")
	iid := FieldOf(inner.Rel(), "id", Int8())
	makeQuery := func(value Expr[int64]) ReadSelect {
		return Select(Project(Output(out, "value", Int8()), ScalarSubquery(SelectValue(iid.Expr()).From(inner).Where(Eq(iid.Expr(), value)).Take(1)))).From(source)
	}
	_, err = TryDefineRead(out, "implicit", makeQuery(id.Expr()))
	require.Error(t, err)
	p, err := TryDefineRead(out, "outer", makeQuery(Outer(id.Expr())))
	require.NoError(t, err)
	sql, _ := p.family.variant(false, nil, "data")
	require.Contains(t, sql.sql, `"_tabula_1"."id" OPERATOR(pg_catalog.=) "_tabula_0"."id"`)
	_, err = TryDefineRead(out, "escaped", makeQuery(Outer(Outer(id.Expr()))))
	require.Error(t, err)
	_, foreign, foreignSource, fid := queryFixture(t)
	_ = foreign
	_, err = TryDefineRead(out, "foreign", Select(Project(Output(out, "value", Int8()), ScalarSubquery(SelectValue(fid.Expr()).From(foreignSource)))))
	require.Error(t, err)
}
func TestQueryLimitsSetsAndCommonTables(t *testing.T) {
	_, out, source, id := queryFixture(t)
	a, b := Input("a", Int8()), Input("b", Int8())
	field := Export("value", a.Expr())
	other := Export("value", b.Expr())
	for _, operation := range []struct {
		name  string
		build func(ReadSelect, ReadSelect, ...ReadSelect) ReadSelect
	}{{"union", Union}, {"union_all", UnionAll}, {"intersect", Intersect}, {"intersect_all", IntersectAll}, {"except", Except}, {"except_all", ExceptAll}} {
		t.Run(operation.name, func(t *testing.T) {
			d := Derived("d", operation.build(Select(field.Project()), Select(other.Project())))
			_, err := TryDefineRead(out, operation.name, Select(Project(Output(out, "value", Int8()), field.From(d))).From(d))
			require.NoError(t, err)
		})
	}
	exported := Export("value", id.Expr())
	common := CTE("notes", Select(exported.Project()).From(source), Materialized)
	d := common.As("n")
	q := Select(Project(Output(out, "value", Int8()), exported.From(d))).From(d)
	_, err := TryDefineRead(out, "missing_with", q)
	require.Error(t, err)
	_, err = TryDefineRead(out, "with", q.With(common))
	require.NoError(t, err)
	_, err = TryDefineRead(out, "duplicate_with", q.With(common, common))
	require.Error(t, err)
	limit := Input("take", Int8())
	p, err := TryDefineRead(out, "limit", Select(Project(Output(out, "value", Int8()), id.Expr())).From(source).Limit(limit, 5))
	require.NoError(t, err)
	shape, _ := p.family.variant(false, nil, "data")
	for _, n := range []int64{-1, 6} {
		_, err = shape.layout.bind(nil, []Binding{BindValue(limit, n)})
		require.Error(t, err)
	}
	_, err = shape.layout.bind(nil, []Binding{BindValue(limit, int64(5))})
	require.NoError(t, err)
	_, err = TryDefineRead(out, "negative_take", q.With(common).Take(-1))
	require.Error(t, err)
}
func TestQueryUnnestAndArrays(t *testing.T) {
	_, out, _, _ := queryFixture(t)
	numbers := Input("values", ArrayOf(Int8()))
	labels := Input("labels", ArrayOf(Text()))
	value := UnnestColumn("value", numbers.Expr(), Int8())
	label := UnnestColumn("label", labels.Expr(), Text())
	position := UnnestOrdinality("position")
	source := Unnest("u", value, label, position)
	p, err := TryDefineRead(out, "unnest", Select(Project(Output(out, "value", Int8()), value.From(source))).From(source))
	require.NoError(t, err)
	shape, _ := p.family.variant(false, nil, "data")
	require.Contains(t, shape.sql, "ROWS FROM (pg_catalog.unnest(")
	require.Contains(t, shape.sql, "WITH ORDINALITY")
	require.True(t, shape.shape.Result[0].Nullable)
	for _, src := range []QuerySource{Unnest("u", Export("value", Input("x", Int8()).Expr())), Unnest("u", position), Unnest("u", value, position, position), Unnest("u", value, value)} {
		_, err = TryDefineRead(out, "invalid", Select(Project(Output(out, "value", Int8()), value.From(src))).From(src))
		require.Error(t, err)
	}
	type arrays struct {
		Value []*int64 `db:"value,array"`
	}
	reg := NewRegistry(RegistryConfig[struct{}]{ArrayColumn: func(reflect.Type) (func(any) any, error) { return func(v any) any { return v }, nil }})
	output := DefineView(reg, ViewSpec[arrays]{Name: "notes"})
	array := ArraySubqueryAs(ArrayOfNullable(Int8()), SelectValue(value.From(source)).From(source))
	_, err = TryDefineRead(output, "array", Select(Project(Output(output, "value", ArrayOfNullable(Int8())), array)))
	require.NoError(t, err)
	bad := ArraySubquery(SelectValue(value.From(source)).From(source))
	type plain struct {
		Value []int64 `db:"value,array"`
	}
	plainOut := DefineView(reg, ViewSpec[plain]{Name: "groups"})
	_, err = TryDefineRead(plainOut, "nullable_array", Select(Project(Output(plainOut, "value", ArrayOf(Int8())), bad)))
	require.ErrorContains(t, err, "ArrayOfNullable")
}
func TestQueryCorrelationAndAggregateOwnership(t *testing.T) {
	_, out, source, id := queryFixture(t)
	inner := SelectValue(SumOf(Outer(id.Expr()), Int8()))
	_, err := TryDefineRead(out, "wrong_aggregate", Select(Project(Output(out, "value", Int8()), ScalarSubquery(inner))).From(source))
	require.ErrorContains(t, err, "owning query")
	value := Export("value", Outer(id.Expr()))
	d := Derived("d", Select(value.Project()))
	project := Select(Project(Output(out, "value", Int8()), value.From(d)))
	_, err = TryDefineRead(out, "nonlateral", project.From(source).CrossJoin(d))
	require.Error(t, err)
	_, err = TryDefineRead(out, "lateral", project.From(source).CrossLateral(d))
	require.NoError(t, err)
	nested := ScalarSubquery(SelectValue(ScalarSubquery(SelectValue(Outer(Outer(id.Expr()))))))
	p, err := TryDefineRead(out, "grandparent", Select(Project(Output(out, "value", Int8()), nested)).From(source))
	require.NoError(t, err)
	shape, _ := p.family.variant(false, nil, "data")
	require.Equal(t, 1, strings.Count(shape.sql, `"public"."items"`))
}

func TestQueryRecursiveScopeAndImmutableMetadata(t *testing.T) {
	_, out, source, id := queryFixture(t)
	value := Export("value", id.Expr())
	var escaped QuerySource
	common := RecursiveCTE("notes", Select(value.Project()).From(source), RecursiveUnionDistinct, func(self QuerySource) ReadSelect {
		escaped = self
		return Select(Export("value", value.From(self)).Project()).From(self)
	})
	c := common.As("n")
	plan, err := TryDefineRead(out, "recursive", Select(Project(Output(out, "value", Int8()), value.From(c))).From(c).With(common))
	require.NoError(t, err)
	shape, _ := plan.family.variant(false, nil, "data")
	require.Contains(t, shape.sql, "WITH RECURSIVE")
	_, err = TryDefineRead(out, "escaped_self", Select(Project(Output(out, "value", Int8()), value.From(escaped))).From(escaped))
	require.Error(t, err)
	_, err = TryDefineRead(out, "escaped_attached_self", Select(Project(Output(out, "value", Int8()), value.From(escaped))).From(escaped).With(common))
	require.ErrorContains(t, err, "escaped its step")
	for _, mode := range []RecursiveUnion{0, RecursiveUnion(99)} {
		bad := RecursiveCTE("notes", Select(value.Project()).From(source), mode, func(self QuerySource) ReadSelect {
			return Select(Export("value", value.From(self)).Project()).From(self)
		})
		n := bad.As("n")
		_, err = TryDefineRead(out, "bad_mode", Select(Project(Output(out, "value", Int8()), value.From(n))).From(n).With(bad))
		require.Error(t, err)
	}
	twice := RecursiveCTE("notes", Select(value.Project()).From(source), RecursiveUnionAll, func(self QuerySource) ReadSelect {
		return Select(Export("value", value.From(self)).Project()).From(self).CrossJoin(self.As("second"))
	})
	n := twice.As("n")
	_, err = TryDefineRead(out, "twice", Select(Project(Output(out, "value", Int8()), value.From(n))).From(n).With(twice))
	require.Error(t, err)
	for _, mode := range []Materialization{DefaultMaterialization, NotMaterialized, Materialized} {
		common := CTE("notes", Select(value.Project()).From(source), mode)
		n := common.As("n")
		p, err := TryDefineRead(out, "mode"+string(rune('a'+mode)), Select(Project(Output(out, "value", Int8()), value.From(n))).From(n).With(common))
		require.NoError(t, err)
		s, _ := p.family.variant(false, nil, "data")
		snapshot := cloneShape(s.shape)
		snapshot.PhysicalColumns[0].Name = "changed"
		require.NotEqual(t, snapshot.PhysicalColumns, s.shape.PhysicalColumns)
	}
	nested := Derived("d", Select(value.Project()).From(source).WithForms(ReadForms{Exact: true}))
	_, err = TryDefineRead(out, "nested_family", Select(Project(Output(out, "value", Int8()), value.From(nested))).From(nested))
	require.Error(t, err)
	badSeed := RecursiveCTE("notes", Select(value.Project()).From(source), RecursiveUnionAll, nil)
	n = badSeed.As("n")
	_, err = TryDefineRead(out, "nil_step", Select(Project(Output(out, "value", Int8()), value.From(n))).From(n).With(badSeed))
	require.Error(t, err)
}

func TestQueryNestedEffectsAndMetadataUnion(t *testing.T) {
	_, out, source, id := queryFixture(t)
	value := Export("value", id.Expr())
	locked := Select(value.Project()).From(source).Lock(ForRows(ForUpdate, source.Rel(), Wait))
	d := Derived("d", locked)
	plan, err := TryDefineRead(out, "locked_derived", Select(Project(Output(out, "value", Int8()), value.From(d))).From(d))
	require.NoError(t, err)
	shape, _ := plan.family.variant(false, nil, "data")
	require.NotZero(t, shape.shape.Effects&EffectRowLock)
	require.Equal(t, shape.shape.Effects, shape.effects.bits)
	require.False(t, plan.single.unbounded)
	require.Equal(t, []string{`"public"."items"`}, shape.shape.AnyColumnUpdate)
	scalar := ScalarSubquery(SelectValue(id.Expr()).From(source))
	inner := scalar.part.(queryExpression)
	inner.query = inner.query.Lock(ForRows(ForUpdate, source.Rel(), Wait))
	scalar.part = inner
	plan, err = TryDefineRead(out, "locked_scalar", Select(Project(Output(out, "value", Int8()), scalar)))
	require.NoError(t, err)
	shape, _ = plan.family.variant(false, nil, "data")
	require.NotZero(t, shape.shape.Effects&EffectRowLock)
	c, err := newTypedCompiler(false)
	require.NoError(t, err)
	meta := StatementShape{Effects: EffectWrite, Access: []ShapeAccess{{Relation: `"public"."items"`, Command: "INSERT"}}, Defaults: []DefaultRequirement{{Relation: `"public"."items"`, Columns: []string{"id"}}}, Result: []ShapeField{{Name: "value", Type: Int8().id.qualified()}}, ResultLineage: []ResultLineage{{Column: "id"}}}
	c.queryShapes = []StatementShape{meta, meta}
	merged := mergeQueryShapes(c, StatementShape{})
	require.Len(t, merged.Access, 1)
	require.Len(t, merged.Defaults, 1)
	require.Empty(t, merged.Result)
	require.Empty(t, merged.ResultLineage)
	merged.Defaults[0].Columns[0] = "label"
	require.Equal(t, "id", meta.Defaults[0].Columns[0])
	_, effects, err := renderCommonTables(&statementContext{compiler: c}, nil)
	require.NoError(t, err)
	require.Zero(t, effects.bits)
}

func TestQueryCommonTableAncestorBarrier(t *testing.T) {
	_, out, source, id := queryFixture(t)
	slot := Input("value", Int8())
	_, err := TryDefineRead(out, "root_outer", Select(Project(Output(out, "value", Int8()), Outer(slot.Expr()))))
	require.Error(t, err)
	// A common-table body cannot see the containing query's local FROM source.
	for _, value := range []Expr[int64]{id.Expr(), Outer(id.Expr())} {
		f := Export("value", value)
		common := CTE("notes", Select(f.Project()), Materialized)
		n := common.As("n")
		_, err = TryDefineRead(out, "local_capture", Select(Project(Output(out, "value", Int8()), f.From(n))).From(source).CrossJoin(n).With(common))
		require.Error(t, err)
	}
	f := Export("value", Outer(Outer(id.Expr())))
	common := CTE("notes", Select(f.Project()), Materialized)
	n := common.As("n")
	result := Export("value", f.From(n))
	d := Derived("d", Select(result.Project()).From(n).With(common))
	q := Select(Project(Output(out, "value", Int8()), result.From(d)))
	_, err = TryDefineRead(out, "explicit_ancestor", q.From(source).CrossLateral(d))
	require.NoError(t, err)
	_, err = TryDefineRead(out, "nonlateral_ancestor", q.From(source).CrossJoin(d))
	require.Error(t, err)
	_, err = TryDefineRead(out, "future_ancestor", q.From(d).CrossJoin(source))
	require.Error(t, err)
}

func TestQueryRejectsRecordAndWrongSetCodecs(t *testing.T) {
	// Pseudotypes cannot become an intermediate schema even without a decoder.
	input := Input("record", CatalogType[any]("pg_catalog", "record", nil))
	record := Export("record", input.Expr())
	_, out, _, _ := queryFixture(t)
	_, err := TryDefineRead(out, "record", Select(Project(Output(out, "value", Int8()), ScalarSubquery(SelectValue(CountAll()).From(Derived("d", Select(record.Project())))))))
	require.Error(t, err)
	a := Input("value", Int8())
	b := Input("value", Int8().WithEncoder(func(n int64) (any, error) { return n, nil }))
	left, right := Export("value", a.Expr()), Export("value", b.Expr())
	source := Derived("d", UnionAll(Select(left.Project()), Select(right.Project())))
	_, err = TryDefineRead(out, "codec", Select(Project(Output(out, "value", Int8()), left.From(source))).From(source))
	require.ErrorContains(t, err, "codecs")
}

func TestQueryLimitAliases(t *testing.T) {
	c, err := newTypedCompiler(false)
	require.NoError(t, err)
	c.params.graph = true
	c.inputAliases = make(map[*typedInput]*typedInput)
	c.inputPrefix = "@row.1."
	input := Input("limit", Int8())
	limit := &queryLimit{input: input.input, ceiling: 3}
	_, err = limit.render(c)
	require.NoError(t, err)
	alias := c.inputAliases[input.input]
	require.NotNil(t, alias)
	_, err = c.params.bind(nil, []Binding{{input: alias, value: int64(4)}})
	require.ErrorContains(t, err, "between zero and 3")
	args, err := c.params.bind(nil, []Binding{{input: alias, value: int64(2)}})
	require.NoError(t, err)
	require.Equal(t, []any{int64(2)}, args)
}

func TestQueryResolvedResultOrigins(t *testing.T) {
	reg, out, source, id := queryFixture(t)
	table := DefinePlanSource(reg, TableSpec[queryItem, int64]{Name: "public.notes", RelationScope: Global()})
	other := MapTable(table).As("n")
	value := Export("value", id.Expr())
	second := Export("value", FieldOf(other.Rel(), "id", Int8()).Expr())
	combined := Derived("d", UnionAll(Select(value.Project()).From(source), Select(second.Project()).From(other)))
	project := Select(Project(Output(out, "value", Int8()), value.From(combined))).From(combined)
	plan, err := TryDefineRead(out, "origins", project)
	require.NoError(t, err)
	shape, err := plan.family.variant(false, nil, "data")
	require.NoError(t, err)
	require.Equal(t, []ResultLineage{{}}, shape.shape.ResultLineage)
	rows, err := project.statementSources()
	require.NoError(t, err)
	c, err := newQueryCompiler(false, reg.statementIssuer, rows)
	require.NoError(t, err)
	c.block, err = newQueryBlock(nil, reg.statementIssuer)
	require.NoError(t, err)
	_, _, _, err = renderQuerySource(&statementContext{compiler: c, block: c.block}, combined, false)
	require.NoError(t, err)
	origins := queryResolvedOrigins(c, value.From(combined).part)
	require.ElementsMatch(t, []physicalColumn{{source.rel.table, "id"}, {other.rel.table, "id"}}, origins)
	direct := Derived("d", Select(value.Project()).From(source))
	plan, err = TryDefineRead(out, "direct_origin", Select(Project(Output(out, "value", Int8()), value.From(direct))).From(direct))
	require.NoError(t, err)
	shape, err = plan.family.variant(false, nil, "data")
	require.NoError(t, err)
	require.Equal(t, []ResultLineage{{Relation: source.rel.table.sql(), Column: "id"}}, shape.shape.ResultLineage)
}
