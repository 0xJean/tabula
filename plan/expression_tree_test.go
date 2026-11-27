package plan

import (
	"github.com/0xJean/tabula"

	"testing"

	"github.com/stretchr/testify/require"
)

func TestMappedReadAndExpressionIdentity(t *testing.T) {
	type item struct {
		ID    int64  `db:"id,key"`
		Label string `db:"label"`
	}
	r := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table := tabula.DefinePlanSource(r, tabula.TableSpec[item, int64]{Name: "public.items", RelationScope: tabula.Global()})
	layout := MapTable(table)
	left, right := layout.As("i"), layout.As("j")
	id := MappedColumn[int64](layout, "id")
	label := MappedColumn[string](layout, "label").On(left.Rel())
	first, second := Input("floor", Int8()), Input("floor", Int8())
	match := func(source RowSource[item, struct{}], s Slot[int64]) Pred {
		return Ge(id.On(source.Rel()).Expr(), s.Expr())
	}
	term := match(left, first)
	require.True(t, sameExpression(term.part, match(left, first).part))
	require.False(t, sameExpression(term.part, match(right, first).part))
	require.False(t, sameExpression(term.part, match(left, second).part))
	require.True(t, repeatableExpression(term.part))
	var scalar Scalar = label.Expr()
	require.Equal(t, typeID(Text()).Qualified(), scalar.scalarOperand().kind.Qualified())
	p := DefineTableRead(left, "labels", SelectColumns(label.Column()), Select().Where(term))
	require.Equal(t, []int{1}, p.indexes)
	def := table.Definition()
	require.Equal(t, `SELECT "i"."label" AS "label" FROM "public"."items" AS "i" WHERE ("i"."id" >= CAST($1 AS "pg_catalog"."int8"))`, def.Statements["labels.global.data"])
	require.Empty(t, r.DefinedViews())
	for _, q := range []ReadSelect{Select().From(left), Select(Projection{}), Select().CrossJoin(right)} {
		bad, err := TryDefineTableRead(left, "bad", SelectAll(), q)
		require.Error(t, err)
		require.Nil(t, bad)
	}
	require.Equal(t, def.Statements, table.Definition().Statements)
}
