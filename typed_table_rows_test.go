package tabula

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTableSelectionOrderAndOwnership(t *testing.T) {
	reg := NewRegistry(RegistryConfig[struct{}]{})
	table := DefinePlanSource(reg, TableSpec[layoutItem, int64]{Name: "public.items", RelationScope: ByRLS()})
	labelType := CatalogType[layoutLabel]("pg_catalog", "text", nil)
	layout := MapTable(table, ColumnType("note", labelType), ColumnType("label", labelType))
	src := layout.As("i")
	id := MappedColumn[int64](layout, "id").On(src.Rel())
	note := MappedColumn[layoutLabel](layout, "note").On(src.Rel())
	arg := Input("id", Int8())
	spec := RowsSpec{RowFilter: RowFilter{Where: Eq(id.Expr(), arg.Expr())}}
	rest := []RowColumn{id.Column()}
	selection := SelectColumns(note.Column(), rest...)
	rest[0] = note.Column()
	p := DefineTableRows(src, "notes", spec, selection)
	require.Equal(t, []int{2, 0}, p.indexes)
	d := table.Definition()
	require.Empty(t, d.Privileges)
	require.Empty(t, reg.DefinedViews())
	require.Equal(t, `SELECT "i"."note" AS "note", "i"."id" AS "id" FROM "public"."items" AS "i" WHERE ("i"."id" = CAST($1 AS "pg_catalog"."int8"))`, d.Statements["notes.rls.data"])
	shape := d.StatementShapes["notes.rls.data"]
	require.Equal(t, []ShapeField{{Name: "note", Type: labelType.id.qualified(), Nullable: true}, {Name: "id", Type: Int8().id.qualified()}}, shape.Result)
	require.Equal(t, []DefinedPrivilege{{Table: `"public"."items"`, Privilege: "SELECT", Columns: []string{"id", "note"}}}, shape.Privileges)
	require.Equal(t, []string{`"public"."items"`}, shape.ForceRLS)
	require.True(t, shape.Inputs[0].Secret)
	count := d.StatementShapes["notes.rls.count"]
	require.False(t, count.Inputs[0].Secret)
	require.Equal(t, []string{"id"}, count.Privileges[0].Columns)

	other := layout.As("i")
	for _, sel := range []TableSelection{{}, SelectColumns(RowColumn{}), SelectColumns(id.Column(), id.Column()),
		SelectColumns(MappedColumn[int64](layout, "id").On(other.Rel()).Column()),
		SelectColumns(FieldOf(src.Rel(), "note", CatalogType[layoutLabel]("pg_catalog", "varchar", nil)).Column()),
	} {
		plan, err := TryDefineTableRows(src, "invalid", spec, sel)
		require.Error(t, err)
		require.Nil(t, plan)
		require.Panics(t, func() { DefineTableRows(src, "invalid", spec, sel) })
	}
	plan, err := TryDefineTableRows(src, "notes", spec, SelectAll())
	require.Error(t, err)
	require.Nil(t, plan)
	_, err = TryDefineTableRows(src, "invalid", spec, SelectAll(), ReadForms{}, ReadForms{})
	require.Error(t, err)
	_, err = TryDefineTableRows(RowSource[layoutItem, struct{}]{}, "invalid", spec, SelectAll())
	require.Error(t, err)
	spec.Budget = 1
	plan, err = TryDefineTableRows(src, "overflow", spec, SelectAll())
	require.Error(t, err)
	require.Nil(t, plan)
	spec.Budget = 0
	spec.Orders = []RowOrder{{Name: "by_id", Terms: []RowTerm{Asc(id)}}}
	spec.Pages = RowPages{Keyset: true, MaxLimit: 10}
	plan, err = TryDefineTableRows(src, "missing_key", spec, SelectColumns(note.Column()))
	require.ErrorContains(t, err, "must be projected")
	require.Nil(t, plan)
	require.Equal(t, d, table.Definition())

	// From RowsFrom, an explicitly typed field can select an otherwise unmapped
	// column. Its catalog type must agree with previously registered shapes.
	rel := FromTable(table, "j", ByRLS())
	raw := RowsFrom(table, rel)
	_, err = TryDefineTableRows(raw, "drift", RowsSpec{}, SelectColumns(FieldOf(rel, "note", CatalogType[layoutLabel]("public", "label", nil)).Column()))
	require.ErrorContains(t, err, "conflicting PostgreSQL types")
	require.Equal(t, d, table.Definition())
	_, err = TryDefineTableRows(raw, "explicit", RowsSpec{}, SelectColumns(FieldOf(rel, "label", labelType).Column()))
	require.NoError(t, err)
}

func TestTableLayoutExactPointerDefaultAndModifier(t *testing.T) {
	reg := NewRegistry(RegistryConfig[struct{}]{DefaultTypes: []TypeDefault{
		TypeMapping(CatalogType[*layoutLabel]("public", "nullable_label", nil)),
		TypeMapping(CatalogType[layoutLabel]("public", "label", nil)),
		TypeMapping(Varchar(8)),
	}})
	table := DefinePlanSource(reg, TableSpec[layoutItem, int64]{Name: "public.items", RelationScope: Global()})
	layout := MapTable(table)
	src := layout.As("i")
	require.Equal(t, "nullable_label", MappedColumn[*layoutLabel](layout, "note").kind.id.name)
	require.Equal(t, "label", MappedColumn[layoutLabel](layout, "label").kind.id.name)
	require.Panics(t, func() { MappedColumn[layoutLabel](layout, "note") })
	plain := CatalogType[string]("pg_catalog", "varchar", nil)
	DefineTableRows(src, "labels", RowsSpec{}, SelectColumns(FieldOf(src.Rel(), "extra", plain).Column()))
	cols := table.Definition().StatementShapes["labels.global.data"].PhysicalColumns
	require.Len(t, cols, 1)
	require.EqualValues(t, 12, *cols[0].TypeMod)
	_, err := TryDefineTableRows(src, "bad_length", RowsSpec{}, SelectColumns(FieldOf(src.Rel(), "extra", Varchar(4)).Column()))
	require.Error(t, err)
}

func TestMappedSecretInputWithoutSourceColumn(t *testing.T) {
	label := CatalogType[layoutLabel]("pg_catalog", "text", nil)
	reg := NewRegistry(RegistryConfig[struct{}]{DefaultTypes: []TypeDefault{TypeMapping(label)}})
	tab := DefinePlanSource(reg, TableSpec[layoutItem, int64]{Name: "public.items", RelationScope: Global()})
	l := MapTable(tab)
	src := l.As("i")
	arg := MappedColumn[layoutLabel](l, "note").Input("value")
	// The secret column supplies only its input metadata. Reading label
	// requires a grant for label and redacts the private comparison value.
	field := MappedColumn[layoutLabel](l, "label").On(src.Rel())
	DefineTableRows(src, "search", RowsSpec{RowFilter: RowFilter{Where: Eq(field.Expr(), arg.Expr())}}, SelectColumns(field.Column()))
	shape := tab.Definition().StatementShapes["search.global.data"]
	require.Equal(t, []string{"label"}, shape.Privileges[0].Columns)
	require.True(t, shape.Inputs[0].Secret)
	type publicRow struct {
		Label layoutLabel `db:"label"`
	}
	out := DefineView(reg, ViewSpec[publicRow]{Name: "entries"})
	DefineRows(out, "echo", src, RowsSpec{}, Project(Output(out, "label", label), arg.Expr()))
	shape = out.Definition().StatementShapes["echo.global.data"]
	require.True(t, shape.Inputs[0].Secret)
	require.Empty(t, shape.Privileges)
	require.Equal(t, []string{`"public"."items"`}, shape.AnyColumnSelect)
}
