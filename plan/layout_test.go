package plan

import (
	"github.com/0xJean/tabula"

	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type layoutLabel string

type layoutItem struct {
	ID       int64        `db:"id,key"`
	Label    layoutLabel  `db:"label"`
	Note     *layoutLabel `db:"note,secret"`
	Rank     int32        `db:"rank,zero"`
	Extra    string       `db:"extra"`
	Optional *string      `db:"optional"`
}

func TestTableLayoutMappings(t *testing.T) {
	labelType := tabula.CatalogType[layoutLabel]("public", "label", nil).WithEncoder(func(s layoutLabel) (any, error) {
		return strings.ToUpper(string(s)), nil
	})
	textDefault := Varchar(18)
	defaults := []tabula.TypeDefault{tabula.TypeMapping(labelType), tabula.TypeMapping(textDefault)}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{DefaultTypes: defaults})
	defaults[0] = tabula.TypeMapping(Int4())
	table := tabula.DefinePlanSource(reg, tabula.TableSpec[layoutItem, int64]{Name: "public.items", RelationScope: tabula.Global()})
	override := []ColumnMapping{ColumnType("extra", Text())}
	layout := MapTable(table, override...)
	override[0] = ColumnType("label", Text())
	left, right := layout.As("i"), layout.As("j")
	require.Same(t, left.Rel().source, left.Rel().source)
	require.NotSame(t, left.Rel().source, right.Rel().source)
	label, note := MappedColumn[layoutLabel](layout, "label"), MappedColumn[layoutLabel](layout, "note")
	require.Equal(t, "public", label.On(left.Rel()).ref.kind.Schema)
	require.False(t, label.On(left.Rel()).Expr().nullable)
	require.True(t, note.On(right.Rel()).Expr().nullable)
	require.True(t, note.On(right.Rel()).ref.secret)
	require.Equal(t, "text", typeID(MappedColumn[string](layout, "extra").kind).Name)
	require.EqualValues(t, 18, typeID(MappedColumn[string](layout, "optional").kind).VarcharLength)
	require.Equal(t, "int4", typeID(MappedColumn[int32](layout, "rank").kind).Name)
	v, w := label.Input("value"), label.Input("value")
	require.NotSame(t, v.input, w.input)
	require.Equal(t, "FIRST", BindValue(v, "first").value)
	require.Equal(t, "SECOND", BindValue(note.Input("note"), "second").value)
	require.Equal(t, "THIRD", At(label.On(right.Rel()), "third").value)
	require.Empty(t, table.Definition().Statements)
	require.Empty(t, table.Definition().Privileges)

	rel := FromTable(table, "items", tabula.Global())
	require.Same(t, rel.source, label.On(rel).ref.source)
	for _, r := range []*tabula.Registry[struct{}]{reg, tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})} {
		other := tabula.DefinePlanSource(r, tabula.TableSpec[layoutItem, int64]{Name: "public.items", RelationScope: tabula.Global()})
		require.Panics(t, func() { label.On(FromTable(other, "i", tabula.Global())) })
	}
	require.Panics(t, func() { label.On(Rel[layoutItem]{}) })
	require.Panics(t, func() { MappedColumn[string](layout, "label") })
	require.Panics(t, func() { MappedColumn[string](layout, "unknown") })
	require.Panics(t, func() { layout.As("") })
	var empty *TableLayout[layoutItem, struct{}]
	require.Panics(t, func() { empty.As("i") })
	require.Panics(t, func() { MappedColumn[string](empty, "extra") })
}

func TestTableLayoutRejectsInvalidDeclarations(t *testing.T) {
	for _, defaults := range [][]tabula.TypeDefault{
		{{}}, {tabula.TypeMapping(tabula.PGType[bool]{})},
		{tabula.TypeMapping(Text()), tabula.TypeMapping(Varchar(5))},
		{tabula.TypeMapping(Int4()), tabula.TypeMapping(Int4())},
	} {
		require.Panics(t, func() { tabula.NewRegistry(tabula.RegistryConfig[struct{}]{DefaultTypes: defaults}) })
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	spec := tabula.TableSpec[layoutItem, int64]{Name: "public.items", RelationScope: tabula.Global()}
	table := tabula.DefinePlanSource(reg, spec)
	for _, overrides := range [][]ColumnMapping{
		{{}}, {ColumnType("bad", Int4())}, {ColumnType("rank", tabula.PGType[int32]{})}, {ColumnType("rank", Int8())},
		{ColumnType("note", Text())}, {ColumnType("extra", Text()), ColumnType("extra", Varchar(4))},
	} {
		layout, err := TryMapTable(table, overrides...)
		require.Error(t, err)
		require.Nil(t, layout)
		require.Panics(t, func() { MapTable(table, overrides...) })
	}
	unregistered, err := tabula.TryDefine(reg, spec)
	require.NoError(t, err)
	for _, invalid := range []*tabula.Table[layoutItem, int64, struct{}]{nil, unregistered,
		tabula.Define(reg, tabula.TableSpec[layoutItem, int64]{Name: "public.notes"}),
		tabula.Define(reg, tabula.TableSpec[layoutItem, int64]{Name: "notes", RelationScope: tabula.Global()}),
	} {
		layout, err := TryMapTable(invalid)
		require.Error(t, err)
		require.Nil(t, layout)
	}
	// An unknown named scalar can be left out of a narrow read.
	layout := MapTable(table)
	src := layout.As("i")
	unused := table.Definition()
	full, err := TryDefineTableRows(src, "all", RowsSpec{}, SelectAll())
	require.Error(t, err)
	require.Nil(t, full)
	require.Equal(t, unused, table.Definition())
	require.Panics(t, func() { MappedColumn[layoutLabel](layout, "note") })
	id := MappedColumn[int64](layout, "id").On(src.Rel())
	narrow := DefineTableRows(src, "by_id", RowsSpec{}, SelectColumns(id.Column()))
	require.NotNil(t, narrow)
	require.Empty(t, table.Definition().Privileges)
}

func TestTableLayoutConventionalTypes(t *testing.T) {
	type plainFields struct {
		ID      int64     `db:"id,key"`
		Small   int16     `db:"small"`
		Rank    int32     `db:"rank"`
		Score   float32   `db:"score"`
		Average float64   `db:"average"`
		Label   string    `db:"label"`
		Enabled bool      `db:"enabled"`
		Bytes   []byte    `db:"data"`
		Updated time.Time `db:"updated_at"`
		Note    *string   `db:"note"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table := tabula.Define(reg, tabula.TableSpec[plainFields, int64]{Name: "public.items", RelationScope: tabula.Global()})
	for _, src := range []RowSource[plainFields, struct{}]{MapTable(table).As("m"), RowsFrom(table, FromTable(table, "r", tabula.Global()))} {
		name := "all_" + src.rel.alias
		DefineTableRows(src, name, RowsSpec{}, SelectAll())
		fields := table.Definition().StatementShapes[name+".global.data"].Result
		want := []string{"int8", "int2", "int4", "float4", "float8", "text", "bool", "bytea", "timestamptz", "text"}
		require.Len(t, fields, len(want))
		for i, typ := range want {
			require.Equal(t, `"pg_catalog".`+typedQuote(typ), fields[i].Type)
			require.Equal(t, i == len(want)-1, fields[i].Nullable)
		}
	}
	type tagged struct {
		ID  int64  `db:"id,key"`
		Doc []byte `db:"body,json"`
	}
	other := tabula.DefinePlanSource(reg, tabula.TableSpec[tagged, int64]{Name: "public.notes", RelationScope: tabula.Global()})
	l := MapTable(other)
	_, err := TryDefineTableRows(l.As("n"), "all", RowsSpec{}, SelectAll())
	require.Error(t, err)
	l = MapTable(other, ColumnType("body", tabula.CatalogType[[]byte]("pg_catalog", "jsonb", nil)))
	require.NotNil(t, DefineTableRows(l.As("n"), "all", RowsSpec{}, SelectAll()))
}

func TestTableLayoutValidatesPrivateLiveValue(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	type toggle struct {
		ID      int64 `db:"id,key"`
		Removed bool  `db:"removed,deleted"`
	}
	toggles := tabula.DefinePlanSource(reg, tabula.TableSpec[toggle, int64]{Name: "public.items", RelationScope: tabula.Global()})
	bad, err := TryMapTable(toggles, ColumnType("removed", tabula.CatalogType[bool]("pg_catalog", "text", nil)))
	require.ErrorContains(t, err, "boolean soft deletion")
	require.Nil(t, bad)
	type record struct {
		ID    int64       `db:"id,key"`
		State layoutLabel `db:"state,deleted"`
	}
	records := tabula.DefinePlanSource(reg, tabula.TableSpec[record, int64]{Name: "public.notes", RelationScope: tabula.Global(),
		SoftDelete: &tabula.SoftDelete{DeletedValue: layoutLabel("removed")}})
	missing, err := TryMapTable(records)
	require.Error(t, err)
	require.Nil(t, missing)
	failure := errors.New("invalid label")
	calls := 0
	kind := tabula.CatalogType[layoutLabel]("public", "label", nil).WithEncoder(func(layoutLabel) (any, error) {
		calls++
		return nil, failure
	})
	invalid, err := TryMapTable(records, ColumnType("state", kind))
	require.ErrorIs(t, err, failure)
	require.Nil(t, invalid)
	require.Equal(t, 1, calls)
	require.Empty(t, records.Definition().Statements)
}

type pointerLayoutSession struct {
	fakeSession
	scope *layoutLabel
}

func (s *pointerLayoutSession) Scope() *layoutLabel { return s.scope }

func TestTableLayoutPointerScope(t *testing.T) {
	type scoped struct {
		ID    int64        `db:"id,key"`
		Scope *layoutLabel `db:"scope_id,scope"`
	}
	calls := 0
	kind := tabula.CatalogType[layoutLabel]("public", "label", nil).WithEncoder(func(v layoutLabel) (any, error) {
		calls++
		return strings.ToUpper(string(v)), nil
	})
	reg := tabula.NewRegistry(tabula.RegistryConfig[*layoutLabel]{DefaultTypes: []tabula.TypeDefault{tabula.TypeMapping(kind)}})
	table := tabula.DefinePlanSource(reg, tabula.TableSpec[scoped, int64]{Name: "public.items"})
	src := MapTable(table).As("i")
	read := DefineTableRows(src, "scoped", RowsSpec{}, SelectAll())
	shape, err := read.family.variant(false, nil, "count")
	require.NoError(t, err)
	s := &pointerLayoutSession{scope: new(layoutLabel("first"))}
	for _, want := range []any{"FIRST", nil} {
		call, err := bindShapeCall(read.owner, shape, s, tabula.CallScan, nil, false, false)
		require.NoError(t, err)
		sql, err := reg.SQLAuthority().Open(s, tabula.CallScan, call)
		require.NoError(t, err)
		require.Equal(t, []any{want}, sql.Args)
		s.scope = nil
	}
	require.Equal(t, 1, calls)
}
