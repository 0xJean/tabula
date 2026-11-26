package tabula

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDistinctSelectionAndWinnerProof(t *testing.T) {
	r := NewRegistry(RegistryConfig[struct{}]{})
	table := DefinePlanSource(r, TableSpec[groupingItem, int64]{Name: "public.items", RelationScope: Global()})
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
	out := DefineView(r, ViewSpec[typedResult]{Name: "entries"})
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
	r := NewRegistry(RegistryConfig[struct{}]{})
	table := DefinePlanSource(r, TableSpec[groupingItem, int64]{Name: "public.items", RelationScope: Global()})
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
	require.Contains(t, table.Definition().Statements["page.global.data.sort.key.after"], `"i"."label" IS NULL AND CAST($1 AS "pg_catalog"."text") IS NOT NULL`)
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
