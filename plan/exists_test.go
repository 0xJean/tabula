package plan

import (
	"github.com/0xJean/tabula"

	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStandaloneExists(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[customTenant]{})
	table := tabula.DefinePlanSource(reg, tabula.TableSpec[typedItem, int64]{Name: "public.items", RelationScope: tabula.ByRLS()})
	rel := FromTable(table, "i", tabula.ByRLS())
	src := RowsFrom(table, rel)
	id, key := FieldOf(rel, "id", Int8()), Input("key", Int8())
	filter := RowFilter{Budget: 2, Observe: []tabula.StatementOption{tabula.Quiet()}, Optional: []RowPredicate{{"id", Eq(id.Expr(), key.Expr())}}}
	plan := DefineExists(src, "present", filter)
	def := table.Definition()
	require.Len(t, def.StatementShapes, 2)
	require.Empty(t, reg.DefinedViews())
	const all, byID = "present.rls.exists", "present.rls.exists.where.id"
	require.Equal(t, `SELECT EXISTS (SELECT 1 FROM "public"."items" AS "i") AS "exists"`, def.Statements[all])
	require.Equal(t, []tabula.ShapeField{{Name: "exists", Type: typeID(Bool()).Qualified()}}, def.StatementShapes[all].Result)
	require.Empty(t, def.StatementShapes[all].Inputs)
	require.Equal(t, []string{`"public"."items"`}, def.StatementShapes[all].AnyColumnSelect)
	require.Equal(t, def.StatementShapes[all].AnyColumnSelect, def.StatementShapes[all].ForceRLS)
	require.Empty(t, def.StatementShapes[byID].AnyColumnSelect)
	require.Equal(t, []string{"id"}, def.StatementShapes[byID].Privileges[0].Columns)
	filter.Budget--
	bad, err := TryDefineExists(src, "too_many", filter)
	require.Nil(t, bad)
	require.ErrorContains(t, err, "exceeds 1 shapes")
	bad, err = TryDefineExists(src, "present", RowFilter{})
	require.Nil(t, bad)
	require.ErrorContains(t, err, "already registered")
	bad, err = TryDefineExists(RowSource[typedItem, customTenant]{}, "absent", RowFilter{})
	require.Nil(t, bad)
	require.Error(t, err)
	require.Equal(t, def, table.Definition())

	s := &routineSession{authority: reg.SQLAuthority(), method: tabula.CallScan}
	bound := plan.Bind(s)
	found, err := bound.Exists(t.Context(), nil)
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, 1, s.calls)
	_, err = bound.Exists(t.Context(), []string{"id"})
	require.Error(t, err)
	_, err = bound.Exists(t.Context(), nil, BindValue(key, 1))
	require.Error(t, err)
	_, err = bound.Exists(t.Context(), []string{"id"}, BindValue(Input("key", Int8()), 1))
	require.Error(t, err)
	_, err = bound.Exists(t.Context(), []string{"id"}, BindValue(key, 1), BindValue(key, 2))
	require.Error(t, err)
	require.Equal(t, 1, s.calls)
	fail := errors.New("boolean scan failed")
	s.scanFn = func(dst ...any) error {
		*dst[0].(*bool) = true
		return fail
	}
	found, err = bound.Exists(t.Context(), []string{"id"}, BindValue(key, 8))
	require.ErrorIs(t, err, fail)
	require.False(t, found)
	require.Equal(t, []any{int64(8)}, s.lastArgs)
	found, err = bad.Bind(s).Exists(t.Context(), nil)
	require.ErrorIs(t, err, tabula.ErrStatementAuthority)
	require.False(t, found)
}

func TestSingleScopedForms(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[customTenant]{})
	rel, src := scopedRowSource(reg, tabula.TableSpec[rowPlanItem, int64]{Name: "public.items"})
	id := FieldOf(rel, "id", Int8())
	key := Input("key", Int8())
	filter := RowFilter{Where: Eq(id.Expr(), key.Expr()), AllowUnscoped: true}
	out := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "entries"})
	rows := DefineRowsWithForms(out, "match", src, RowsSpec{RowFilter: filter}, ReadForms{Exact: true, First: UnorderedFirst},
		Project(Output(out, "id", Int8()), id.Expr()))
	probe := DefineExists(src, "visible", filter)
	require.Contains(t, tableDef(reg, "public.items").StatementShapes, "visible.scoped.exists")
	require.Contains(t, tableDef(reg, "public.items").StatementShapes, "visible.unscoped.exists")
	require.True(t, out.Definition().StatementShapes["match.unscoped.single_first"].UnscopedOnly)
	s := &routineSession{authority: reg.SQLAuthority(), method: tabula.CallScan}
	s.tenant = "alpha"
	_, err := probe.Bind(s).Exists(t.Context(), nil, BindValue(key, 4))
	require.NoError(t, err)
	require.Equal(t, []any{customTenant("alpha"), int64(4)}, s.lastArgs)
	require.Contains(t, s.lastQuery, `"i"."removed" = FALSE`)
	s.method, s.unscoped = tabula.CallEach, true
	_, _, err = rows.Bind(s).MaybeOne(t.Context(), RowChoice{}, BindValue(key, 4))
	require.NoError(t, err)
	require.Equal(t, []any{int64(4), int64(2)}, s.lastArgs)
	require.NotContains(t, s.lastQuery, "scope_id")
}
