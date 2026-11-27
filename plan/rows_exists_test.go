package plan

import (
	"github.com/0xJean/tabula"

	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRowsExistsRegistration(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[customTenant]{})
	table := tabula.Define(reg, tabula.TableSpec[typedItem, int64]{Name: "public.items", RelationScope: tabula.ByRLS()})
	rel := FromTable(table, "i", tabula.ByRLS())
	src := RowsFrom(table, rel)
	id := FieldOf(rel, "id", Int8())
	label := FieldOf(rel, "label", Text())
	type result struct {
		Label *string `db:"label"`
	}
	out := tabula.DefineView(reg, tabula.ViewSpec[result]{Name: "notes"})
	project := Project(Output(out, "label", Text()), label.Expr())
	key := Input("key", Int8())
	spec := RowsSpec{
		WithExists: true, RowFilter: RowFilter{Budget: 6, Optional: []RowPredicate{{"by_id", IsDistinct(id.Expr(), key.Expr())}}},
		Orders: []RowOrder{{Name: "forward", Terms: []RowTerm{Asc(id)}}},
		Pages:  RowPages{Offset: true, MaxLimit: 1},
	}
	plan := DefineRows(out, "search", src, spec, project)
	snapshot := out.Definition()
	require.Len(t, snapshot.StatementShapes, 6)
	const noFilter = "search.rls.exists"
	const filtered = noFilter + ".where.by_id"
	require.Equal(t, `SELECT EXISTS (SELECT 1 FROM "public"."items" AS "i") AS "exists"`, snapshot.Statements[noFilter])
	require.Contains(t, snapshot.Statements[filtered], `WHERE ("i"."id" IS DISTINCT FROM CAST($1 AS "pg_catalog"."int8"))`)
	all := snapshot.StatementShapes[noFilter]
	require.Equal(t, []tabula.ShapeField{{Name: "exists", Type: typeID(Bool()).Qualified()}}, all.Result)
	require.Equal(t, []string{`"public"."items"`}, all.AnyColumnSelect)
	require.Equal(t, all.AnyColumnSelect, all.ForceRLS)
	require.Empty(t, all.Privileges)
	require.Empty(t, all.Inputs)
	byID := snapshot.StatementShapes[filtered]
	require.Equal(t, []tabula.ShapeInput{{Name: "key", Type: typeID(Int8()).Qualified()}}, byID.Inputs)
	require.Equal(t, []string{"id"}, byID.Privileges[0].Columns)
	require.Equal(t, []tabula.ShapeAccess{{Relation: `"public"."items"`, Command: "SELECT"}}, byID.Access)

	s := &routineSession{authority: reg.SQLAuthority(), method: tabula.CallScan}
	s.scanFn = func(dest ...any) error { *dest[0].(*bool) = true; return nil }
	bound := plan.Bind(s)
	found, err := bound.Exists(t.Context(), []string{"by_id"}, BindValue(key, 9))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, []any{int64(9)}, s.lastArgs)
	_, err = bound.Exists(t.Context(), nil, BindValue(key, 9))
	require.Error(t, err)
	_, err = bound.Exists(t.Context(), []string{"unknown"})
	require.Error(t, err)
	require.Equal(t, 1, s.calls)
	failed := errors.New("scan failed")
	s.scanFn = func(dest ...any) error { *dest[0].(*bool) = true; return failed }
	found, err = bound.Exists(t.Context(), nil)
	require.ErrorIs(t, err, failed)
	require.False(t, found)

	spec.Budget--
	_, err = TryDefineRows(out, "budget", src, spec, project)
	require.ErrorContains(t, err, "budget")
	spec = RowsSpec{WithExists: true, RowFilter: RowFilter{Where: IsDistinct(id.Expr(), Input("wrong", tabula.CatalogType[int64]("public", "rank", nil)).Expr())}}
	_, err = TryDefineRows(out, "mismatch", src, spec, project)
	require.Error(t, err)
	require.Equal(t, snapshot, out.Definition())
}

func TestRowsSummaryAvailability(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[customTenant]{})
	table := tabula.Define(reg, tabula.TableSpec[typedItem, int64]{Name: "public.items", RelationScope: tabula.Global()})
	rel := FromTable(table, "i", tabula.Global())
	src := RowsFrom(table, rel)
	out := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "notes"})
	project := Project(Output(out, "id", Int8()), FieldOf(rel, "id", Int8()).Expr())
	s := &routineSession{authority: reg.SQLAuthority(), method: tabula.CallEach}
	ctx := t.Context()
	plain := DefineRows(out, "plain", src, RowsSpec{}, project)
	_, err := plain.Bind(s).Exists(ctx, nil)
	require.ErrorIs(t, err, tabula.ErrOperationDenied)
	locked := DefineRows(out, "locked", src, RowsSpec{Lock: ForRows(ForUpdate, rel, SkipLocked)}, project).Bind(s)
	_, err = locked.Count(ctx, nil)
	require.ErrorIs(t, err, tabula.ErrOperationDenied)
	_, _, err = locked.List(ctx, RowChoice{})
	require.ErrorIs(t, err, tabula.ErrOperationDenied)
	_, err = locked.Exists(ctx, nil)
	require.ErrorIs(t, err, tabula.ErrOperationDenied)
	_, err = TryDefineRows(out, "aggregate", src, RowsSpec{WithExists: true}, Project(Output(out, "id", Int8()), CountAll()))
	require.ErrorContains(t, err, "WithExists")
	require.Zero(t, s.calls)
}
