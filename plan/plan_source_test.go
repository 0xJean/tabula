package plan

import (
	"testing"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

type modeItem struct {
	ID       int64        `db:"id,key,serial"`
	Scope    customTenant `db:"scope_id,scope"`
	Label    string       `db:"label"`
	Rank     int64        `db:"rank"`
	Revision int          `db:"revision,version"`
	Removed  bool         `db:"removed,deleted"`
}

func TestPlanSourceDeclaration(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[customTenant]{})
	spec := tabula.TableSpec[modeItem, int64]{Name: "public.items", WriteMode: tabula.WriteUpdateOnly, UpdateColumns: []string{"rank", "revision"}}
	legacy := tabula.Define(reg, spec).Definition()
	source, err := tabula.TryDefinePlanSource(reg, spec)
	require.NoError(t, err)
	d := source.Definition()
	require.Equal(t, legacy.Columns, d.Columns)
	require.Empty(t, d.Statements)
	require.Empty(t, d.Privileges)
	require.Empty(t, source.Insert())
	require.Equal(t, []tabula.Definition{legacy, d}, reg.Defined())

	rel := FromTable(source, "i", tabula.ByScopeColumn())
	scope := FieldOf(rel, "scope_id", tabula.CatalogType[customTenant]("pg_catalog", "text", nil))
	src := RowsFrom(source, rel, scope.Column())
	id, rank := FieldOf(rel, "id", Int8()), FieldOf(rel, "rank", Int8())
	rev := FieldOf(rel, "revision", tabula.CatalogType[int]("pg_catalog", "int8", nil))
	slot := Input("value", Int8())
	filter := RowFilter{Where: Eq(id.Expr(), slot.Expr())}
	mutation := MutationSpec{RowFilter: filter, Version: ExpectVersion(rev, Input("version", tabula.CatalogType[int]("pg_catalog", "int8", nil)))}
	_, err = TryDefineUpdateMany(src, "change", mutation, RowSet{"rank", []RowAssignment{Assign(rank, slot.Expr())}})
	require.NoError(t, err)
	key := "change.scoped.set.rank"
	d = source.Definition()
	require.Len(t, d.Statements, 1)
	require.Contains(t, d.Statements[key], `"i"."removed" = FALSE`)
	require.Contains(t, d.Statements[key], `"i"."scope_id"`)
	require.Contains(t, d.Statements[key], `"i"."revision" + 1`)
	var read []string
	for _, p := range d.StatementShapes[key].Privileges {
		if p.Privilege == "SELECT" {
			read = p.Columns
		}
	}
	require.ElementsMatch(t, []string{"id", "scope_id", "removed", "revision"}, read)
	label := FieldOf(rel, "label", Text())
	_, err = TryDefineUpdateMany(src, "denied", mutation, RowSet{"label", []RowAssignment{Assign(label, Input("label", Text()).Expr())}})
	require.ErrorIs(t, err, tabula.ErrOperationDenied)
	_, err = TryDefineDeleteMany(src, "remove", mutation)
	require.ErrorIs(t, err, tabula.ErrOperationDenied)
	require.Equal(t, d, source.Definition())
	require.Equal(t, legacy, reg.Defined()[0])
}

func TestPlanSourceTypedWriteLimits(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	for _, spec := range []tabula.TableSpec[typedItem, int64]{
		{Name: "public.items", RelationScope: tabula.Global(), ReadOnly: true},
		{Name: "public.items", RelationScope: tabula.Global(), WriteMode: tabula.WriteInsertOnly},
		{Name: "public.items", RelationScope: tabula.Global(), AllowDelete: true},
	} {
		table := tabula.DefinePlanSource(reg, spec)
		rel := FromTable(table, "i", tabula.Global())
		id := FieldOf(rel, "id", Int8())
		src := RowsFrom(table, rel)
		where := RowFilter{Where: Eq(id.Expr(), Input("id", Int8()).Expr())}
		_, err := TryDefineDeleteMany(src, "remove", MutationSpec{RowFilter: where})
		switch {
		case spec.ReadOnly:
			require.ErrorIs(t, err, tabula.ErrReadOnly)
		case spec.AllowDelete:
			require.NoError(t, err)
			require.Len(t, table.Definition().Statements, 1)
		default:
			require.ErrorIs(t, err, tabula.ErrOperationDenied)
			out := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "entries"})
			_, err = TryDefineRows(out, "search", src, RowsSpec{}, Project(Output(out, "id", Int8()), id.Expr()))
			require.ErrorIs(t, err, tabula.ErrOperationDenied)
			require.Empty(t, out.Definition().Statements)
		}
	}
}

type groupKey struct {
	GroupID int64 `db:"group_id,key"`
	ItemID  int64 `db:"item_id,key"`
}

func TestPlanSourceCompositeKeyLock(t *testing.T) {
	for _, mode := range []tabula.WriteMode{tabula.WriteDefault, tabula.WriteUpdateOnly} {
		reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{Deny: tabula.DenyLegacySQL})
		table := tabula.DefinePlanSource(reg, tabula.TableSpec[groupKey, groupKey]{Name: "public.groups", RelationScope: tabula.Global(), WriteMode: mode})
		rel := FromTable(table, "g", tabula.Global())
		id := FieldOf(rel, "item_id", Int8())
		out := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "entries"})
		DefineRows(out, "hold", RowsFrom(table, rel), RowsSpec{Lock: ForRows(ForUpdate, rel, NoWait)},
			Project(Output(out, "id", Int8()), id.Expr()))
		d := out.Definition()
		require.Len(t, d.Statements, 1)
		require.Contains(t, d.Statements["hold.global.data"], `FOR UPDATE OF "g" NOWAIT`)
		require.Equal(t, []string{`"public"."groups"`}, d.StatementShapes["hold.global.data"].AnyColumnUpdate)
		require.Equal(t, []tabula.DefinedPrivilege{{Table: `"public"."groups"`, Privilege: "SELECT", Columns: []string{"item_id"}}}, d.StatementShapes["hold.global.data"].Privileges)
		require.Empty(t, table.Definition().Statements)
	}
}
