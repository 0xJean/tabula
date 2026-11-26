package tabula

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTypedInputEncoderAndStorage(t *testing.T) {
	rejected := errors.New("invalid value")
	var original, replacement int
	base := CatalogType[int64]("pg_catalog", "int8", func(n int64) any { original++; return n + 1 })
	modified := base.WithEncoder(func(n int64) (any, error) {
		replacement++
		if n < 0 {
			return nil, rejected
		}
		return n + 2, nil
	})
	a, b := Input("a", base), Input("b", modified)
	args := []Binding{BindValue(b, 3), BindValue(a, 3)}
	layout := newParameterLayout(false)
	for _, in := range []*typedInput{a.input, b.input, b.input} {
		_, err := layout.parameter(in)
		require.NoError(t, err)
	}
	for range 2 {
		got, err := layout.bind(nil, args)
		require.NoError(t, err)
		require.Equal(t, []any{int64(4), int64(5)}, got)
	}
	require.Equal(t, 1, original)
	require.Equal(t, 1, replacement)
	args[0] = BindValue(b, -1)
	for range 2 {
		got, err := layout.bind(nil, args)
		require.Nil(t, got)
		require.ErrorIs(t, err, rejected)
	}
	require.Equal(t, 2, replacement)
	require.Panics(t, func() { base.WithEncoder(nil) })
	require.Panics(t, func() { PGType[int64]{}.WithEncoder(func(n int64) (any, error) { return n, nil }) })

	bytesType := CatalogType[[]byte]("pg_catalog", "bytea", nil)
	data := Input("data", bytesType)
	member := Input("member", ArrayOf(bytesType))
	storage := newParameterLayout(false)
	_, err := storage.parameter(data.input)
	require.NoError(t, err)
	_, err = storage.parameter(member.input)
	require.NoError(t, err)
	binary := []byte{0, 127}
	members := [][]byte{binary, {}, nil}
	bound := []Binding{BindValue(data, binary), BindValue(member, members)}
	binary[0], members[1] = 3, []byte{9}
	for range 2 {
		values, err := storage.bind(nil, bound)
		require.NoError(t, err)
		require.Equal(t, []any{[]byte{0, 127}, [][]byte{{0, 127}, {}, nil}}, values)
		values[0].([]byte)[0] = 6
		values[1].([][]byte)[0][0] = 8
		values[1].([][]byte)[2] = []byte{10}
	}
	type document []byte
	docs := Input("documents", ArrayOf(CatalogType[document]("pg_catalog", "jsonb", nil)))
	raw := []document{[]byte("null")}
	doc := BindValue(docs, raw)
	raw[0][0] = 'x'
	require.Equal(t, []document{document("null")}, doc.value)

	named := Input("binary", CatalogType[document]("pg_catalog", "bytea", nil))
	source := document{0, 1}
	byteBinding := BindValue(named, source)
	namedLayout := newParameterLayout(false)
	_, err = namedLayout.parameter(named.input)
	require.NoError(t, err)
	source[0] = 9
	for range 2 {
		driverArgs, err := namedLayout.bind(nil, []Binding{byteBinding})
		require.NoError(t, err)
		require.Equal(t, document{0, 1}, driverArgs[0])
		driverArgs[0].(document)[0] = 7
	}
	require.Nil(t, BindValue(named, nil).value)
	require.Equal(t, document{}, BindValue(named, document{}).value)
}

func TestNullableArrayBindingSnapshotsEachStatement(t *testing.T) {
	type document []byte
	element := CatalogType[document]("pg_catalog", "bytea", nil)
	slot := Input("documents", ArrayOfNullable(element))
	raw := document{0, 7}
	values := []*document{&raw, nil, new(document{}), new(document)}
	b := BindValue(slot, values)
	raw[0], values[1] = 3, &raw
	store := newParameterLayout(false)
	_, err := store.parameter(slot.input)
	require.NoError(t, err)
	for range 2 {
		args, err := store.bind(nil, []Binding{b})
		require.NoError(t, err)
		got := args[0].([]*document)
		require.Equal(t, []*document{new(document{0, 7}), nil, new(document{}), new(document)}, got)
		(*got[0])[1] = 9
		got[1] = got[0]
		*got[2] = append(*got[2], 1)
	}
	require.Panics(t, func() { ArrayOfNullable(PGType[string]{}) })
	require.Panics(t, func() { ArrayOfNullable(ArrayOf(Int4())) })
}

func TestScopeEncoderIdentity(t *testing.T) {
	type scoped struct {
		ID    int64        `db:"id,key"`
		Scope customTenant `db:"scope_id,scope"`
	}
	raw := CatalogType[customTenant]("pg_catalog", "text", nil)
	calls := 0
	withPrefix := func(prefix string) PGType[customTenant] {
		return raw.WithEncoder(func(v customTenant) (any, error) {
			calls++
			return prefix + string(v), nil
		})
	}
	a, b := withPrefix("one"), withPrefix("two")
	for _, explicit := range []bool{false, true} {
		cfg := RegistryConfig[customTenant]{}
		if explicit {
			cfg.ScopeID = func(v customTenant) any { return "explicit" + string(v) }
		}
		reg := NewRegistry(cfg)
		left := DefinePlanSource(reg, TableSpec[scoped, int64]{Name: "public.items"})
		right := DefinePlanSource(reg, TableSpec[scoped, int64]{Name: "public.jobs"})
		view := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
		from := func(table *Table[scoped, int64, customTenant], alias string, kind PGType[customTenant]) RowSource[scoped, customTenant] {
			return MapTable(table, ColumnType("scope_id", kind)).As(alias)
		}
		l, r := from(left, "l", a), from(right, "r", b)
		register := func(name string, first, second RowSource[scoped, customTenant]) (*ReadPlan[typedResult, customTenant], error) {
			project := Project(Output(view, "id", Int8()), FieldOf(first.Rel(), "id", Int8()).Expr())
			return TryDefineRead(view, name, Select(project).From(first).CrossJoin(second))
		}
		s := &routineSession{authority: reg.SQLAuthority(), method: CallEach}
		s.tenant = "-scope"
		before := calls
		for _, test := range []struct {
			name          string
			first, second RowSource[scoped, customTenant]
		}{{"forward", l, r}, {"reverse", r, l}, {"mixed", l, from(right, "r", raw)}} {
			prior := view.Definition()
			p, err := register(test.name, test.first, test.second)
			if !explicit {
				require.ErrorContains(t, err, "share an encoder")
				require.Nil(t, p)
				require.Equal(t, prior, view.Definition())
				continue
			}
			require.NoError(t, err)
			_, err = p.Bind(s).Find(t.Context())
			require.NoError(t, err)
			require.Equal(t, []any{"explicit-scope"}, s.lastArgs)
		}
		require.Equal(t, before, calls)
		p, err := register("shared", l, from(right, "r", a))
		require.NoError(t, err)
		_, err = p.Bind(s).Find(t.Context())
		require.NoError(t, err)
		if explicit {
			require.Equal(t, []any{"explicit-scope"}, s.lastArgs)
			require.Equal(t, before, calls)
		} else {
			require.Equal(t, []any{"one-scope"}, s.lastArgs)
			require.Equal(t, before+1, calls)
		}
	}
}
