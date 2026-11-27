package plan

import (
	"github.com/0xJean/tabula"

	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadListBudgetAndFixedCompatibility(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[int32]{})
	table := tabula.DefinePlanSource(reg, tabula.TableSpec[joinItem, int64]{Name: "public.items"})
	src := FromTable(table, "i", tabula.ByScopeColumn())
	id := FieldOf(src, "id", Int8())
	out := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "entries"})
	query := Select(Project(Output(out, "id", Int8()), id.Expr())).From(src).OrderBy(Asc(id)).WithForms(ReadForms{Exact: true, First: OrderedFirst}).AllowUnscoped()
	DefineRead(out, "fixed", query)
	spec := ReadListSpec{Optional: []RowPredicate{{"a", IsNull(id.Expr())}, {"b", IsNull(id.Expr())}}, Orders: []RowOrder{{"asc", []RowTerm{Asc(id)}}, {"desc", []RowTerm{Desc(id)}}}, Pages: RowPages{Offset: true, Keyset: true, MaxLimit: 10, MaxOffset: 20}, ExactCount: true, Budget: 60}
	plan := DefineRead(out, "listed", query.WithList(spec))
	def := out.Definition()
	require.Len(t, def.Statements, 4+60)
	for _, scope := range []string{"scoped", "unscoped"} {
		for _, form := range []string{"data", "single_first"} {
			require.Equal(t, def.Statements["fixed."+scope+"."+form], def.Statements["listed."+scope+"."+form])
			require.Equal(t, def.StatementShapes["fixed."+scope+"."+form], def.StatementShapes["listed."+scope+"."+form])
		}
	}
	spec.Budget = 59
	_, err := TryDefineRead(out, "excess", query.WithList(spec))
	require.ErrorContains(t, err, "59 shapes")
	require.ErrorContains(t, err, "filter.a=2")
	require.Equal(t, def, out.Definition())
	_, err = plan.Cursor("asc", At(id, int64(1)))
	require.NoError(t, err)
	_, err = plan.Cursor("missing", At(id, int64(1)))
	require.Error(t, err)
	for _, bad := range []ReadSelect{
		query.WithList(spec).WithList(spec),
		query.WithList(ReadListSpec{Optional: []RowPredicate{{"same", IsNull(id.Expr())}, {"same", IsNull(id.Expr())}}}),
		query.WithList(ReadListSpec{Orders: []RowOrder{{"same", []RowTerm{Asc(id)}}, {"same", []RowTerm{Desc(id)}}}}),
		query.WithList(ReadListSpec{Pages: RowPages{Offset: true, MaxLimit: 1}}),
		query.WithList(ReadListSpec{Budget: -1}),
		query.Lock(ForRows(ForUpdate, src, Wait)).WithList(ReadListSpec{ExactCount: true}),
	} {
		_, err := TryDefineRead(out, "bad", bad)
		require.Error(t, err)
		require.Equal(t, def, out.Definition())
	}
}

func TestReadListDefaultBudgetBoundary(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table := tabula.DefinePlanSource(reg, tabula.TableSpec[typedItem, int64]{Name: "public.items", RelationScope: tabula.Global()})
	src := FromTable(table, "i", tabula.Global())
	id := FieldOf(src, "id", Int8())
	out := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "entries"})
	base := Select(Project(Output(out, "id", Int8()), id.Expr())).From(src)
	spec := ReadListSpec{}
	for i := range 255 {
		spec.Orders = append(spec.Orders, RowOrder{fmt.Sprintf("o%d", i), []RowTerm{Asc(id)}})
	}
	DefineRead(out, "exact", base.WithList(spec))
	def := out.Definition()
	require.Len(t, def.Statements, 256)
	spec.Orders = append(spec.Orders, RowOrder{"one_more", []RowTerm{Asc(id)}})
	_, err := TryDefineRead(out, "excess", base.WithList(spec))
	require.ErrorContains(t, err, "256 shapes")
	require.Equal(t, def, out.Definition())
}

func TestReadListJoinedProofAndCursor(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[int32]{})
	table := tabula.DefinePlanSource(reg, tabula.TableSpec[joinItem, int64]{Name: "public.items"})
	a, b := FromTable(table, "a", tabula.ByScopeColumn()), FromTable(table, "b", tabula.ByScopeColumn())
	aid, bid := FieldOf(a, "id", Int8()), FieldOf(b, "id", Int8())
	type result struct {
		Left  int64  `db:"left"`
		Right *int64 `db:"right"`
	}
	out := tabula.DefineView(reg, tabula.ViewSpec[result]{Name: "entries"})
	base := Select(Project(Output(out, "left", Int8()), aid.Expr()), Project(Output(out, "right", Int8()), bid.Expr())).From(a).LeftJoin(b, Eq(aid.Expr(), bid.Expr()))
	spec := ReadListSpec{Orders: []RowOrder{{"keys", []RowTerm{Desc(aid).NullsLast(), Asc(bid).NullsFirst()}}}, Pages: RowPages{Keyset: true, MaxLimit: 5}, ExactCount: true}
	plan := DefineRead(out, "joined", base.WithList(spec))
	_, err := plan.Cursor("keys", At(aid, int64(1)), NullAt(bid))
	require.NoError(t, err)
	cursor, err := plan.CursorFrom("keys", result{Left: 1})
	require.NoError(t, err)
	require.Nil(t, cursor.bindings[1].value)
	_, err = plan.Cursor("keys", At(bid, int64(1)), NullAt(aid))
	require.Error(t, err)
	def := out.Definition()
	shape := def.StatementShapes["joined.scoped.list.data.sort.keys.after"]
	require.Equal(t, []tabula.KeyRequirement{{Relation: `"public"."items"`, Columns: []string{"id"}}}, shape.Keys)
	require.Contains(t, def.Statements["joined.scoped.list.data.sort.keys.after"], "OPERATOR(pg_catalog.<)")
	require.Empty(t, def.StatementShapes["joined.scoped.list.count"].Keys)
	shape.Keys[0].Columns[0] = "changed"
	require.Equal(t, "id", out.Definition().StatementShapes["joined.scoped.list.data.sort.keys.after"].Keys[0].Columns[0])
	spec.Orders[0].Terms = []RowTerm{Asc(aid)}
	_, err = TryDefineRead(out, "incomplete", base.WithList(spec))
	require.ErrorContains(t, err, "complete key")
	require.Equal(t, def.Statements, out.Definition().Statements)
}

func TestReadListPairValidationAndIndependentBindings(t *testing.T) {
	calls := 0
	reg := tabula.NewRegistry(tabula.RegistryConfig[customTenant]{ArrayArg: func(v any) any { calls++; values := v.([]int64); values[0] = 99; return "adapted" }})
	rel, src := scopedRowSource(reg, tabula.TableSpec[rowPlanItem, int64]{Name: "public.items"})
	id := FieldOf(rel, "id", Int8())
	out := tabula.DefineView(reg, tabula.ViewSpec[rowPlanResult]{Name: "entries"})
	ids, extra := Input("ids", ArrayOf(Int8())), Input("extra", Int4())
	plan := DefineRead(out, "list", Select(Project(Output(out, "id", Int8()), id.Expr()), Project(Output(out, "rank", Int4()), extra.Expr())).From(src).
		Where(Any(id.Expr(), ids.Expr())).WithList(ReadListSpec{ExactCount: true}))
	session := &rowPlanSession{authority: reg.SQLAuthority()}
	bound := plan.Bind(session)
	vals := []Binding{BindValue(ids, []int64{1, 2}), BindValue(extra, int32(4))}
	rows, total, err := bound.List(t.Context(), RowChoice{}, vals...)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.EqualValues(t, 8, total)
	require.Equal(t, 2, calls)
	require.Equal(t, []int64{1, 2}, vals[0].value)
	require.Len(t, session.calls, 2)
	require.Len(t, session.calls[0].Args, 2) // scope and ids
	require.Len(t, session.calls[1].Args, 3)
	for _, bad := range [][]Binding{nil, vals[:1], {vals[0], vals[1], vals[0]}, {BindValue(Input("ids", ArrayOf(Int8())), []int64{1}), vals[1]}, {vals[0], {input: extra.input, err: errors.New("encode")}}} {
		before := len(session.calls)
		rows, total, err = bound.List(t.Context(), RowChoice{}, bad...)
		require.Error(t, err)
		require.Empty(t, rows)
		require.Zero(t, total)
		require.Len(t, session.calls, before)
	}
	session.failure = errors.New("database")
	rows, total, err = bound.List(t.Context(), RowChoice{}, vals...)
	require.Error(t, err)
	require.Nil(t, rows)
	require.Zero(t, total)
	_, err = bound.Count(t.Context(), nil, vals...)
	require.Error(t, err) // projection-only binding is extra for a standalone count
}
