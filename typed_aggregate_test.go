package tabula

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAggregateModifiers(t *testing.T) {
	value, delimiter := Input("value", Text()), Input("delimiter", Text())
	flag := Input("enabled", Bool())
	agg := FilterWhere(AggregateOrder(DistinctInputs(StringAgg(value.Expr(), delimiter.Expr())), DescExpr(value.Expr()).NullsLast()), flag.Expr())
	c, err := newTypedCompiler(false)
	require.NoError(t, err)
	sql, err := agg.part.render(c)
	require.NoError(t, err)
	require.Equal(t, `pg_catalog.string_agg(DISTINCT CAST($1 AS "pg_catalog"."text"), CAST($2 AS "pg_catalog"."text") ORDER BY CAST($1 AS "pg_catalog"."text") DESC NULLS LAST) FILTER (WHERE CAST($3 AS "pg_catalog"."bool"))`, sql)
	require.Contains(t, c.functions, `pg_catalog.string_agg("pg_catalog"."text","pg_catalog"."text")`)
	require.True(t, agg.nullable)
	require.True(t, sameExpression(agg.part, FilterWhere(AggregateOrder(DistinctInputs(StringAgg(value.Expr(), delimiter.Expr())), DescExpr(value.Expr()).NullsLast()), flag.Expr()).part))
	require.False(t, sameExpression(agg.part, FilterWhere(AggregateOrder(DistinctInputs(StringAgg(value.Expr(), delimiter.Expr())), AscExpr(value.Expr())), flag.Expr()).part))
	require.False(t, sameExpression(CountOf(value.Expr()).part, CountDistinct(value.Expr()).part))
	require.False(t, repeatableExpression(JSONAgg(CatalogType[string]("pg_catalog", "jsonb", nil), value.Expr()).part))
	require.False(t, repeatableExpression(ArrayAgg(value.Expr()).part))
	require.True(t, repeatableExpression(AggregateOrder(ArrayAgg(value.Expr()), AscExpr(value.Expr())).part))
	for _, part := range []expressionPart{
		FilterWhere(value.Expr(), flag.Expr()).part,
		FilterWhere(FilterWhere(CountAll(), flag.Expr()), flag.Expr()).part,
		DistinctInputs(DistinctInputs(CountOf(value.Expr()))).part,
		DistinctInputs(CountAll()).part,
		AggregateOrder(CountAll(), AscExpr(value.Expr())).part,
		AggregateOrder(AggregateOrder(StringAgg(value.Expr(), delimiter.Expr()), AscExpr(value.Expr())), AscExpr(value.Expr())).part,
		AggregateOrder(DistinctInputs(StringAgg(value.Expr(), delimiter.Expr())), AscExpr(flag.Expr())).part,
		FilterWhere(CountAll(), Eq(CountAll(), CountAll())).part,
		CountOf(CountAll()).part,
		FilterWhere(CountAll(), Input("bad", CatalogType[bool]("pg_catalog", "text", nil)).Expr()).part,
		ArrayAgg(Expr[int32]{}).part,
		ArrayAggAs(Text(), value.Expr()).part,
		JSONBAgg(CatalogType[string]("pg_catalog", "json", nil), value.Expr()).part,
		BoolAnd(Input("bad", CatalogType[bool]("pg_catalog", "text", nil)).Expr()).part,
	} {
		c, err := newTypedCompiler(false)
		require.NoError(t, err)
		_, err = part.render(c)
		require.Error(t, err, "%T", part)
	}
}

func TestAggregateNullElementsAndTypes(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	rel, _ := scopedRowSource(reg, TableSpec[rowPlanItem, int64]{Name: "public.items"})
	label, rank := FieldOf(rel, "label", Text()), FieldOf(rel, "rank", Int4())
	c, err := newTypedCompiler(false, rel.source)
	require.NoError(t, err)
	_, err = ArrayAgg(label.Expr()).part.render(c)
	require.ErrorContains(t, err, "array-of-nullable")
	_, err = ArrayAgg(rank.Expr()).part.render(c)
	require.NoError(t, err)
	require.ErrorContains(t, checkAggregateNulls(ArrayAgg(rank.Expr()).part, map[*occurrence]bool{rel.source: true}), "array-of-nullable")
	for _, part := range []expressionPart{BoolOr(IsNull(label.Expr())).part, BoolAnd(IsNull(label.Expr())).part,
		JSONAgg(CatalogType[string]("pg_catalog", "json", nil), label.Expr()).part,
		JSONAgg(CatalogType[string]("pg_catalog", "jsonb", nil), rank.Expr()).part,
		CountDistinct(rank.Expr()).part} {
		_, err := part.render(c)
		require.NoError(t, err)
	}
	json := Input("doc", CatalogType[string]("pg_catalog", "json", nil))
	_, err = CountDistinct(json.Expr()).part.render(c)
	require.ErrorContains(t, err, "comparable")
}

type groupingItem struct {
	ID    int64   `db:"id,key"`
	Label *string `db:"label"`
	Rank  int32   `db:"rank"`
}

type groupingResult struct {
	Missing bool  `db:"missing"`
	Count   int64 `db:"count"`
}

func TestComputedGroupingAndOuterSeek(t *testing.T) {
	r := NewRegistry(RegistryConfig[struct{}]{})
	table := DefinePlanSource(r, TableSpec[groupingItem, int64]{Name: "public.items", RelationScope: Global()})
	src := MapTable(table).As("i")
	label := FieldOf(src.Rel(), "label", Text())
	key := IsNull(label.Expr())
	out := DefineView(r, ViewSpec[groupingResult]{Name: "groups"})
	cols := []Projection{Project(Output(out, "missing", Bool()), key), Project(Output(out, "count", Int8()), CountAll())}
	plan := DefineRows(out, "grouped", src, RowsSpec{GroupBy: []RowColumn{GroupExpr(key)}, Orders: []RowOrder{{"key", []RowTerm{DescExpr(CountAll()), AscExpr(key)}}}, Pages: RowPages{Keyset: true, MaxLimit: 4}}, cols...)
	statement := out.Definition().Statements["grouped.global.data.sort.key.after"]
	require.Contains(t, statement, `GROUP BY ("i"."label" IS NULL)) AS "rows" WHERE`)
	require.Contains(t, statement, `ORDER BY "rows"."count" DESC NULLS FIRST, "rows"."missing" ASC NULLS LAST`)
	row := groupingResult{Missing: true, Count: 5}
	manual, err := plan.Cursor("key", AtExpr(CountAll(), int64(5)), AtExpr(key, true))
	require.NoError(t, err)
	decoded, err := plan.CursorFrom("key", row)
	require.NoError(t, err)
	require.Equal(t, manual, decoded)
	_, err = TryDefineRead(out, "read", Select(cols...).From(src).GroupBy(key).Having(Ge(CountAll(), Input("floor", Int8()).Expr())).OrderBy(AscExpr(Not(key))))
	require.NoError(t, err)
	badCols := []Projection{Project(Output(out, "missing", Bool()), Eq(label.Expr(), label.Expr())), cols[1]}
	_, err = TryDefineRows(out, "bad", src, RowsSpec{GroupBy: []RowColumn{GroupExpr(key)}}, badCols...)
	require.ErrorContains(t, err, "ungrouped")
	for i, q := range []ReadSelect{
		Select(cols...).From(src).GroupBy(CountAll()),
		Select(cols...).From(src).GroupBy(key, key),
		Select(cols...).From(src).GroupBy(key).Where(Eq(CountAll(), CountAll())),
		Select(cols...).From(src).GroupBy(key).OrderBy(Asc(label)),
		Select(cols...).From(src).GroupBy(Expr[bool]{}),
	} {
		_, err := TryDefineRead(out, fmt.Sprintf("bad_%d", i), q)
		require.Error(t, err)
	}
	_, err = TryDefineTableRows(src, "bad_columns", RowsSpec{}, SelectColumns(GroupExpr(key)))
	require.Error(t, err)
}
