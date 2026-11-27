package plan

import (
	"github.com/0xJean/tabula"

	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadListAggregateOrderProof(t *testing.T) {
	type item struct {
		ID        int64   `db:"id,key"`
		Group     int32   `db:"group_id"`
		Label     string  `db:"label"`
		Separator string  `db:"separator"`
		Rank      float64 `db:"rank"`
		Number    string  `db:"number"`
	}
	type result struct {
		Group int32   `db:"group_id"`
		Value *string `db:"value"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table := tabula.DefinePlanSource(reg, tabula.TableSpec[item, int64]{Name: "public.items", RelationScope: tabula.Global()})
	src := FromTable(table, "i", tabula.Global())
	group, label, separator := FieldOf(src, "group_id", Int4()), FieldOf(src, "label", Text()), FieldOf(src, "separator", Text())
	out := tabula.DefineView(reg, tabula.ViewSpec[result]{Name: "entries"})
	build := func(name string, value Expr[string]) (*ReadPlan[result, struct{}], error) {
		return TryDefineRead(out, name, Select(Project(Output(out, "group_id", Int4()), group.Expr()), Project(Output(out, "value", Text()), value)).From(src).GroupBy(group.Expr()).
			WithList(ReadListSpec{Orders: []RowOrder{{"value", []RowTerm{AscExpr(value), Asc(group)}}}, Pages: RowPages{Keyset: true, MaxLimit: 2}}))
	}
	unresolved := AggregateOrder(StringAgg(label.Expr(), separator.Expr()), Asc(group))
	_, err := build("rank_ties", unresolved)
	require.ErrorContains(t, err, "cursor aggregate order must determine every input")
	_, err = build("delimiter_ties", AggregateOrder(StringAgg(label.Expr(), separator.Expr()), Asc(label)))
	require.ErrorContains(t, err, "cursor aggregate order must determine every input")
	good := AggregateOrder(StringAgg(label.Expr(), separator.Expr()), Asc(label), Asc(separator))
	_, err = build("determined", good)
	require.NoError(t, err)
	shape := out.Definition().StatementShapes["determined.global.list.data.sort.value.after"]
	require.Empty(t, shape.Keys)
	require.Contains(t, shape.Comparisons, tabula.ComparisonRequirement{Relation: `"public"."items"`, Column: "separator"})
	require.Contains(t, shape.Comparisons, tabula.ComparisonRequirement{Relation: `"public"."items"`, Column: "label", WithRelation: `"public"."items"`, WithColumn: "separator"})
	comparisons, err := readOrderComparisons([]RowTerm{AscExpr(FilterWhere(CountAll(), Eq(label.Expr(), separator.Expr())))})
	require.NoError(t, err)
	require.Contains(t, comparisons, tabula.ComparisonRequirement{Relation: `"public"."items"`, Column: "label", WithRelation: `"public"."items"`, WithColumn: "separator"})
	comparisons, err = readOrderComparisons([]RowTerm{Asc(label), Asc(separator)})
	require.NoError(t, err)
	require.Len(t, comparisons, 2)
	// Unproved aggregation remains valid as an ordinary fixed read.
	_, err = TryDefineRead(out, "fixed", Select(Project(Output(out, "group_id", Int4()), group.Expr()), Project(Output(out, "value", Text()), unresolved)).From(src).GroupBy(group.Expr()).OrderBy(AscExpr(unresolved)))
	require.NoError(t, err)
	rank := FieldOf(src, "rank", tabula.CatalogType[float64]("pg_catalog", "float8", nil))
	type total struct {
		Group int32    `db:"group_id"`
		Value *float64 `db:"value"`
	}
	sums := tabula.DefineView(reg, tabula.ViewSpec[total]{Name: "totals"})
	sum := SumOf(rank.Expr(), tabula.CatalogType[float64]("pg_catalog", "float8", nil))
	_, err = TryDefineRead(sums, "unstable_sum", Select(Project(Output(sums, "group_id", Int4()), group.Expr()), Project(Output(sums, "value", tabula.CatalogType[float64]("pg_catalog", "float8", nil)), sum)).From(src).GroupBy(group.Expr()).
		WithList(ReadListSpec{Orders: []RowOrder{{"sum", []RowTerm{AscExpr(sum), Asc(group)}}}, Pages: RowPages{Keyset: true, MaxLimit: 2}}))
	require.ErrorContains(t, err, "floating aggregate")
	for _, aggregate := range []Expr[float64]{MinOf(rank.Expr()), MaxOf(rank.Expr())} {
		_, err = readOrderComparisons([]RowTerm{AscExpr(aggregate)})
		require.ErrorContains(t, err, "equal input's representative")
	}
	number := FieldOf(src, "number", tabula.CatalogType[string]("pg_catalog", "numeric", nil))
	_, err = readOrderComparisons([]RowTerm{AscExpr(MinOf(number.Expr()))})
	require.ErrorContains(t, err, "equal input's representative")
	// Only a cursor/winner needs this proof; ordinary numeric MIN remains legal.
	_, err = TryDefineRead(sums, "fixed_min", Select(Project(Output(sums, "group_id", Int4()), group.Expr()), Project(Output(sums, "value", tabula.CatalogType[float64]("pg_catalog", "float8", nil)), MinOf(rank.Expr()))).From(src).GroupBy(group.Expr()))
	require.NoError(t, err)
	_, err = TryDefineRead(sums, "winner_min", Select(Project(Output(sums, "group_id", Int4()), group.Expr()), Project(Output(sums, "value", tabula.CatalogType[float64]("pg_catalog", "float8", nil)), MinOf(rank.Expr()))).From(src).GroupBy(group.Expr()).DistinctOn(group.Expr()).OrderBy(Asc(group), AscExpr(MinOf(rank.Expr()))))
	require.ErrorContains(t, err, "DISTINCT ON winner")
	require.ErrorContains(t, err, "equal input's representative")
	_, err = readOrderComparisons([]RowTerm{AscExpr(MinOf(group.Expr()))})
	require.NoError(t, err)
	_, err = readOrderComparisons([]RowTerm{AscExpr(MinOf(Input("value", tabula.CatalogType[float64]("pg_catalog", "float8", nil)).Expr()))})
	require.NoError(t, err)
	_, err = readOrderComparisons([]RowTerm{AscExpr(AggregateOrder(ArrayAgg(rank.Expr()), Asc(rank)))})
	require.ErrorContains(t, err, "equal input's representative")
}

func TestReadListCompositeProofAndDisabledCalls(t *testing.T) {
	type pair struct {
		Region int32 `db:"region,key"`
		ID     int64 `db:"id,key"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table := tabula.DefinePlanSource(reg, tabula.TableSpec[pair, pair]{Name: "public.items", RelationScope: tabula.Global()})
	src := MapTable(table).As("i")
	region, id := FieldOf(src.Rel(), "region", Int4()), FieldOf(src.Rel(), "id", Int8())
	spec := ReadListSpec{Orders: []RowOrder{{"key", []RowTerm{Asc(id)}}}, Pages: RowPages{Keyset: true, MaxLimit: 5}}
	_, err := TryDefineTableRead(src, "incomplete", SelectAll(), Select().WithList(spec))
	require.ErrorContains(t, err, "complete key requires i.region")
	spec.Orders[0].Terms = append(spec.Orders[0].Terms, Desc(region))
	plan, err := TryDefineTableRead(src, "complete", SelectColumns(id.Column(), region.Column()), Select().WithList(spec))
	require.NoError(t, err)
	cursor, err := plan.CursorFrom("key", pair{Region: 3, ID: 5})
	require.NoError(t, err)
	require.Equal(t, int64(5), cursor.bindings[0].value)
	require.Equal(t, int32(3), cursor.bindings[1].value)
	var nilPlan *ReadPlan[pair, struct{}]
	_, err = nilPlan.CursorFrom("key", pair{})
	require.Error(t, err)
	_, err = nilPlan.Cursor("key")
	require.Error(t, err)
	var bound *BoundReadPlan[pair, struct{}]
	_, err = bound.Page(t.Context(), RowChoice{})
	require.ErrorIs(t, err, tabula.ErrStatementAuthority)
	_, _, err = bound.List(t.Context(), RowChoice{})
	require.ErrorIs(t, err, tabula.ErrStatementAuthority)
	_, err = bound.Count(t.Context(), nil)
	require.ErrorIs(t, err, tabula.ErrStatementAuthority)
}
