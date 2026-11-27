package plan

import (
	"github.com/0xJean/tabula"

	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDistinctSelectionAndWinnerProof(t *testing.T) {
	r := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table := tabula.DefinePlanSource(r, tabula.TableSpec[groupingItem, int64]{Name: "public.items", RelationScope: tabula.Global()})
	layout := MapTable(table)
	src := layout.As("i")
	id, rank := FieldOf(src.Rel(), "id", Int8()), FieldOf(src.Rel(), "rank", Int4())
	label := FieldOf(src.Rel(), "label", Text())
	_, err := TryDefineTableRead(src, "distinct", SelectColumns(label.Column()), Select().Distinct().OrderBy(Asc(label)))
	require.NoError(t, err)
	require.Equal(t, `SELECT DISTINCT "i"."label" AS "label" FROM "public"."items" AS "i" ORDER BY "i"."label" ASC NULLS LAST`, table.Definition().Statements["distinct.global.data"])
	_, err = TryDefineTableRead(src, "winner", SelectAll(), Select().DistinctOn(label.Expr()).OrderBy(Asc(label).NullsFirst(), Desc(rank), Asc(id)))
	require.NoError(t, err)
	require.Contains(t, table.Definition().Statements["winner.global.data"], `DISTINCT ON ("i"."label")`)
	for i, q := range []ReadSelect{
		Select().Distinct().Distinct(),
		Select().Distinct().DistinctOn(label.Expr()),
		Select().Distinct().OrderBy(Asc(rank)),
		Select().DistinctOn(label.Expr()).OrderBy(Asc(label)),
		Select().DistinctOn(label.Expr()).OrderBy(Asc(id), Asc(label)),
		Select().DistinctOn(label.Expr(), label.Expr()).OrderBy(Asc(label), Asc(id)),
		Select().DistinctOn(label.Expr()).DistinctOn(label.Expr()),
		Select().DistinctOn(nil),
		Select().DistinctOn(label.Expr()).OrderBy(Asc(label), Asc(id)).Lock(ForRows(ForUpdate, src.Rel(), Wait)),
	} {
		_, err := TryDefineTableRead(src, fmt.Sprintf("bad_%d", i), SelectColumns(label.Column()), q)
		require.Error(t, err, "%d", i)
	}
	other := layout.As("n")
	otherID := FieldOf(other.Rel(), "id", Int8())
	out := tabula.DefineView(r, tabula.ViewSpec[typedResult]{Name: "entries"})
	query := Select(Project(Output(out, "id", Int8()), id.Expr())).From(src).CrossJoin(other).DistinctOn(id.Expr()).OrderBy(Asc(id))
	_, err = TryDefineRead(out, "bad_join", query)
	require.ErrorContains(t, err, "complete key")
	_, err = TryDefineRead(out, "joined_winner", query.OrderBy(Asc(otherID)))
	require.NoError(t, err)
}

type unstableOrdering struct{ part expressionPart }

func (n unstableOrdering) render(c *typedCompiler) (string, error) { return n.part.render(c) }
func (n unstableOrdering) nullableAfterJoin(outer map[*occurrence]bool) bool {
	return nullExtended(n.part, outer)
}
func (n unstableOrdering) expressionTraits() expressionTraits {
	return expressionTraits{identity: "test/stable", children: []expressionPart{n.part}, stability: expressionStable}
}

func TestComputedOrderCursorMapping(t *testing.T) {
	r := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table := tabula.DefinePlanSource(r, tabula.TableSpec[groupingItem, int64]{Name: "public.items", RelationScope: tabula.Global()})
	src := MapTable(table).As("i")
	id, label := FieldOf(src.Rel(), "id", Int8()), FieldOf(src.Rel(), "label", Text())
	spec := RowsSpec{Orders: []RowOrder{{"key", []RowTerm{DescExpr(label.Expr()).NullsLast(), Asc(id)}}}, Pages: RowPages{Keyset: true, MaxLimit: 5}}
	plan := DefineTableRows(src, "page", spec, SelectColumns(label.Column(), id.Column()))
	row := groupingItem{ID: 7, Label: new("b"), Rank: 100}
	manual, err := plan.Cursor("key", AtExpr(label.Expr(), "b"), At(id, 7))
	require.NoError(t, err)
	decoded, err := plan.CursorFrom("key", row)
	require.NoError(t, err)
	require.Equal(t, manual, decoded)
	_, err = plan.Cursor("key", AtExpr(Input("label", Text()).Expr(), "b"), At(id, 7))
	require.Error(t, err)
	row.Label = nil
	manual, err = plan.Cursor("key", NullAtExpr(label.Expr()), At(id, 7))
	require.NoError(t, err)
	decoded, err = plan.CursorFrom("key", row)
	require.NoError(t, err)
	require.Equal(t, manual, decoded)
	def := table.Definition()
	for _, key := range []string{"page.global.data.sort.key.first", "page.global.data.sort.key.after"} {
		meta := def.StatementShapes[key]
		require.Equal(t, []tabula.KeyRequirement{{Relation: `"public"."items"`, Columns: []string{"id"}}}, meta.Keys)
		require.Len(t, meta.Comparisons, 2)
	}
	require.Empty(t, def.StatementShapes["page.global.count"].Keys)
	require.Contains(t, def.Statements["page.global.data.sort.key.after"], `"i"."label" OPERATOR(pg_catalog.<) CAST($1 AS "pg_catalog"."text")`)
	_, err = plan.CursorFrom("unknown", row)
	require.Error(t, err)
	unstable := label.Expr()
	unstable.part = unstableOrdering{unstable.part}
	spec.Orders[0].Terms[0] = AscExpr(unstable)
	_, err = TryDefineTableRows(src, "unstable", spec, SelectAll())
	require.ErrorContains(t, err, "repeatable")
	_, err = TryDefineTableRead(src, "unstable_winner", SelectAll(), Select().DistinctOn(label.Expr()).OrderBy(Asc(label), AscExpr(unstable), Asc(id)))
	require.ErrorContains(t, err, "repeatable")
	failure := errors.New("encode failed")
	encoded := Input("custom", Text().WithEncoder(func(string) (any, error) { return nil, failure }))
	_, err = plan.Cursor("key", AtExpr(encoded.Expr(), "b"), At(id, 7))
	require.ErrorIs(t, err, failure)
}

func TestPhysicalCatalogNameKeepsFieldOrdering(t *testing.T) {
	type item struct {
		ID    int64  `db:"id,key"`
		Label string `db:"label"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table := tabula.DefinePlanSource(reg, tabula.TableSpec[item, int64]{Name: "public.items", RelationScope: tabula.Global()})
	layout := MapTable(table, ColumnType("label", tabula.CatalogType[string]("pg_catalog", "name", nil)))
	src := layout.As("i")
	label := MappedColumn[string](layout, "label").On(src.Rel())
	spec := RowsSpec{GroupBy: []RowColumn{label.Column()}, Orders: []RowOrder{{Name: "label", Terms: []RowTerm{Asc(label)}}}}
	_, err := TryDefineTableRows(src, "names", spec, SelectColumns(label.Column()))
	require.NoError(t, err)
	want := `SELECT "i"."label" AS "label" FROM "public"."items" AS "i" GROUP BY "i"."label" ORDER BY "i"."label" ASC NULLS LAST`
	require.Equal(t, want, table.Definition().Statements["names.global.data.sort.label"])
	query := Select().GroupBy(label.Expr()).OrderBy(Asc(label))
	_, err = TryDefineTableRead(src, "fixed", SelectColumns(label.Column()), query)
	require.NoError(t, err)
	require.Equal(t, want, table.Definition().Statements["fixed.global.data"])
}

func TestQuotedKeyDoesNotCollideWithGeneratedCursorName(t *testing.T) {
	type item struct {
		ID   int64 `db:"id,key"`
		Rank int64 `db:"id.3"`
	}
	r := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table := tabula.Define(r, tabula.TableSpec[item, int64]{Name: "public.items", RelationScope: tabula.Global(), ReadOnly: true})
	layout := MapTable(table)
	key := MappedColumn[int64](layout, "id")
	fields := []Field[int64]{
		MappedColumn[int64](layout, "id.3").On(layout.As("a").Rel()),
		key.On(layout.As("b").Rel()),
		key.On(layout.As("c").Rel()),
	}
	var order rowOrdering
	var projections []Projection
	var values []CursorValue
	for i, field := range fields {
		order.terms = append(order.terms, Asc(field))
		projections = append(projections, Projection{index: i, part: field.ref})
		values = append(values, At(field, int64(i+1)))
	}
	require.NoError(t, prepareOrderCursor(&order, projections, nil))
	params := newParameterLayout(false)
	for _, input := range order.cursors {
		_, err := params.parameter(input)
		require.NoError(t, err)
	}
	cursor, err := cursorForOrder(&order, values)
	require.NoError(t, err)
	args, err := params.bind(nil, cursor.bindings)
	require.NoError(t, err)
	require.Equal(t, []any{int64(1), int64(2), int64(3)}, args)
}

func TestRowSummariesRequireStableMembership(t *testing.T) {
	r := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table := tabula.DefinePlanSource(r, tabula.TableSpec[groupingItem, int64]{Name: "public.items", RelationScope: tabula.Global()})
	src := MapTable(table).As("i")
	condition := Eq(GenRandomUUID(), GenRandomUUID())
	for _, filter := range []RowFilter{
		{Where: condition},
		{Optional: []RowPredicate{{Name: "match", When: condition}}},
	} {
		_, err := TryDefineTableRows(src, "sample", RowsSpec{RowFilter: filter}, SelectAll())
		require.ErrorContains(t, err, "volatile")
		require.Empty(t, table.Definition().StatementShapes)
	}
	_, err := TryDefineTableRead(src, "read", SelectAll(), Select().Where(condition))
	require.NoError(t, err)
}
