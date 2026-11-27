package tabula

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWindowNativeFamilies(t *testing.T) {
	i, b := Input("value", Int4()).Expr(), Input("offset", Int4()).Expr()
	spec := WindowSpec{Partition: []Scalar{i}, Order: []RowTerm{DescExpr(i).NullsLast()}, Frame: WholePartition()}
	for _, value := range []Scalar{
		Over(RowNumber(), spec), Over(Rank(), spec), Over(DenseRank(), spec), Over(PercentRank(), spec), Over(CumeDist(), spec),
		Over(NTile(b), spec), Over(Lag(i, b, i), spec), Over(Lead(i, b, i), spec),
		Over(FirstValue(i), spec), Over(LastValue(i), spec), Over(NthValue(i, b), spec),
		Over(CountAll(), spec), Over(FilterWhere(CountOf(i), Gt(i, b)), spec),
		Over(SumOf(i, Int8()), spec), Over(MinOf(i), spec), Over(MaxOf(i), spec), Over(BoolAnd(Eq(i, i)), spec),
		Over(ArrayAgg(i), spec), Over(StringAgg(Cast(Text(), i), Cast(Text(), b)), spec),
	} {
		v := value.scalarOperand()
		c, err := newTypedCompiler(false)
		require.NoError(t, err)
		query, err := v.part.render(c)
		require.NoError(t, err)
		require.Contains(t, query, `OVER (PARTITION BY CAST($`)
		require.Contains(t, query, `DESC NULLS LAST ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)`)
		require.NoError(t, checkResultExpr(v.part, func(expressionPart) error { return nil }))
		require.ErrorContains(t, checkRowExpr(v.part, nil), "window outside")
		require.NotEmpty(t, c.functions)
		require.False(t, hasAggregate(v.part))
		require.True(t, sameExpression(v.part, v.part))
	}
	for _, v := range []Scalar{Over(RowNumber(), spec), Over(Rank(), spec), Over(DenseRank(), spec), Over(PercentRank(), spec), Over(CumeDist(), spec), Over(CountAll(), spec), Over(Lag(i, b, i), spec)} {
		require.False(t, v.scalarOperand().nullable)
	}
	for _, v := range []Scalar{Over(FirstValue(i), spec), Over(LastValue(i), spec), Over(NthValue(i, b), spec), Over(Lead(i, b, TypedNull(Int4())), spec)} {
		require.True(t, v.scalarOperand().nullable)
	}
}

func TestWindowFrameGrammar(t *testing.T) {
	offset := Input("distance", Int8()).Expr()
	for _, mode := range []FrameMode{FrameRows, FrameRange, FrameGroups} {
		for _, exclusion := range []FrameExclusion{ExcludeNoOthers, ExcludeCurrentRow, ExcludeGroup, ExcludeTies} {
			frame := WindowFrame{Mode: mode, Start: Preceding(offset), End: Following(offset), Exclude: exclusion}
			c, err := newTypedCompiler(false)
			require.NoError(t, err)
			query, err := Over(CountAll(), WindowSpec{Order: []RowTerm{AscExpr(offset)}, Frame: frame}).part.render(c)
			require.NoError(t, err)
			require.Contains(t, query, `BETWEEN CAST($1 AS "pg_catalog"."int8") PRECEDING AND CAST($1 AS "pg_catalog"."int8") FOLLOWING`)
			require.Len(t, c.params.order, 1)
			if mode == FrameRange {
				require.Contains(t, c.functions, "pg_catalog.in_range(int8,int8,int8,boolean,boolean)")
			}
		}
	}
	for _, frame := range []WindowFrame{
		{Mode: 255}, {Mode: FrameRows}, {Mode: FrameRows, Start: UnboundedFollowing(), End: UnboundedFollowing()},
		{Mode: FrameRows, Start: CurrentRow(), End: Preceding(offset)},
		{Mode: FrameRows, Start: Following(offset), End: CurrentRow()},
		{Mode: FrameRows, Start: UnboundedPreceding(), End: UnboundedPreceding()},
		{Mode: FrameRows, Start: Preceding(Input("small", Int4()).Expr()), End: CurrentRow()},
		{Mode: FrameGroups, Start: CurrentRow(), End: CurrentRow()},
		{Mode: FrameRange, Start: Preceding(offset), End: CurrentRow()},
		{Mode: FrameRows, Start: CurrentRow(), End: CurrentRow(), Exclude: 255},
		{Start: CurrentRow()}, {Exclude: ExcludeGroup},
	} {
		c, err := newTypedCompiler(false)
		require.NoError(t, err)
		_, err = Over(CountAll(), WindowSpec{Frame: frame}).part.render(c)
		require.Error(t, err, "%+v", frame)
	}
	// Reversed distances in the same category can validly describe an empty
	// frame; no comparison of runtime bind values belongs to SQL generation.
	require.NoError(t, (WindowFrame{Mode: FrameRows, Start: Preceding(offset), End: Preceding(offset)}).validate(nil))
	for _, pair := range []struct{ order, offset catalogType }{
		{Int4().id, Int8().id}, {CatalogType[int16]("pg_catalog", "int2", nil).id, Int4().id}, {Timestamptz().id, CatalogType[string]("pg_catalog", "interval", nil).id},
		{CatalogType[float32]("pg_catalog", "float4", nil).id, CatalogType[float64]("pg_catalog", "float8", nil).id},
	} {
		require.NotEmpty(t, windowRangeSignature(pair.order, pair.offset))
	}
	require.Empty(t, windowRangeSignature(Int8().id, Int4().id))
	require.Empty(t, windowRangeSignature(Text().id, Int8().id))
}

func TestWindowPhaseAndGrouping(t *testing.T) {
	reg := NewRegistry(RegistryConfig[struct{}]{})
	type item struct {
		ID int64 `db:"id,key"`
	}
	tab := DefinePlanSource(reg, TableSpec[item, int64]{Name: "public.items", RelationScope: Global()})
	src := MapTable(tab).As("i")
	id := FieldOf(src.Rel(), "id", Int8())
	type result struct {
		Value *int64 `db:"value"`
	}
	out := DefineView(reg, ViewSpec[result]{Name: "entries"})
	spec := WindowSpec{Order: []RowTerm{Asc(id)}}
	w := Over(RowNumber(), spec)
	base := Select(Project(Output(out, "value", Int8()), w)).From(src)
	_, err := TryDefineRead(out, "ranked", base)
	require.NoError(t, err)
	_, err = TryDefineRead(out, "ordered", base.OrderBy(DescExpr(w)))
	require.NoError(t, err)
	before := out.Definition()
	for _, bad := range []ReadSelect{
		base.Where(Gt(w, id.Expr())), base.GroupBy(w), base.Having(Gt(w, id.Expr())),
		Select(Project(Output(out, "value", Int8()), CountOf(w))).From(src),
		Select(Project(Output(out, "value", Int8()), Over(FirstValue(w), spec))).From(src),
		Select(Project(Output(out, "value", Int8()), Over(CountOf(id.Expr()), WindowSpec{Order: []RowTerm{AscExpr(w)}}))).From(src),
		Select(Project(Output(out, "value", Int8()), Over(DistinctInputs(CountOf(id.Expr())), spec))).From(src),
		Select(Project(Output(out, "value", Int8()), Over(CountAll(), WindowSpec{Frame: WindowFrame{Mode: FrameRows, Start: Preceding(id.Expr()), End: CurrentRow()}}))).From(src),
		base.GroupBy(Input("group", Int4()).Expr()),
	} {
		_, err := TryDefineRead(out, "bad", bad)
		require.Error(t, err)
		require.Equal(t, before, out.Definition())
	}
	agg := Over(SumOf(CountAll(), CatalogType[string]("pg_catalog", "numeric", nil)), WindowSpec{Order: []RowTerm{Asc(id)}})
	require.True(t, hasAggregate(agg.part))
	require.NoError(t, checkResultExpr(agg.part, func(expressionPart) error { return nil }))
	c, err := newTypedCompiler(false, src.rel)
	require.NoError(t, err)
	_, err = agg.part.render(c)
	require.NoError(t, err)
	_, err = Over(RowNumber(), WindowSpec{Partition: []Scalar{nil}}).part.render(c)
	require.Error(t, err)
	_, err = RowNumber().part.render(c)
	require.ErrorContains(t, err, "requires Over")
	_, err = Over(AggregateOrder(ArrayAgg(id.Expr()), Asc(id)), spec).part.render(c)
	require.ErrorContains(t, err, "aggregate-local ORDER BY")
}

func TestWindowNamedSpecificationAndCopy(t *testing.T) {
	i := Input("value", Int8()).Expr()
	spec := WindowSpec{Name: "ranked", Partition: []Scalar{i}, Order: []RowTerm{AscExpr(i)}, Frame: RunningRows()}
	a, b := Over(RowNumber(), spec), Over(Rank(), spec)
	spec.Partition[0] = TypedNull(Int8())
	spec.Order[0] = DescExpr(i)
	query := Select(Projection{part: a.part}, Projection{part: b.part})
	c, err := newTypedCompiler(false)
	require.NoError(t, err)
	sql, err := query.windowClause(c)
	require.NoError(t, err)
	require.Equal(t, ` WINDOW "ranked" AS (PARTITION BY CAST($1 AS "pg_catalog"."int8") ORDER BY CAST($1 AS "pg_catalog"."int8") ASC NULLS LAST ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)`, sql)
	_, err = Select(Projection{part: a.part}, Projection{part: Over(Rank(), spec).part}).windowClause(c)
	require.ErrorContains(t, err, "conflicting named window")
}
