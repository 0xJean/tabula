//go:build integration

package tabula

// RLSReadFixture compiles a one-column read for integration tests of the
// renderer, verification metadata and driver receipt. Test exports are absent
// from the library and from the generated public reference.
func RLSReadFixture(table string) (Definition, SQLAuthority, func(Session[struct{}], int64) (AuthorizedStatement, error), error) {
	reg := NewRegistry(RegistryConfig[struct{}]{})
	tab := Define(reg, TableSpec[typedItem, int64]{Name: table, RelationScope: ByRLS(), ReadOnly: true})
	src := FromTable(tab, "i", ByRLS())
	out := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	c, err := newTypedCompiler(true, src.source)
	if err != nil {
		return Definition{}, SQLAuthority{}, nil, err
	}
	id := FieldOf(src, "id", Int8())
	input := Input("id", Int8())
	columns, result, err := c.project(out.entry, []Projection{Project(Output(out, "id", Int8()), id.Expr())})
	if err != nil {
		return Definition{}, SQLAuthority{}, nil, err
	}
	where, err := Eq(id.Expr(), input.Expr()).part.render(c)
	if err != nil {
		return Definition{}, SQLAuthority{}, nil, err
	}
	meta, err := c.readRequirements()
	if err != nil {
		return Definition{}, SQLAuthority{}, nil, err
	}
	meta.Result = result
	sql := "SELECT " + columns[0] + " FROM " + src.source.table.sql() + " AS " + typedQuote(src.source.alias) + " WHERE " + where
	shape, err := c.finishShape("by_id", sql, catalogType{}, meta)
	if err != nil {
		return Definition{}, SQLAuthority{}, nil, err
	}
	if err := out.registerShapes("by_id", []*compiledShape{shape}); err != nil {
		return Definition{}, SQLAuthority{}, nil, err
	}
	bind := func(s Session[struct{}], value int64) (AuthorizedStatement, error) {
		return reg.bindCall(shape, s, CallScan, []Binding{BindValue(input, value)})
	}
	return Definition(out.Definition()), reg.SQLAuthority(), bind, nil
}
