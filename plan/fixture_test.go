//go:build integration

package plan

import "github.com/0xJean/tabula"

// RLSReadFixture compiles a one-column read for integration tests of the
// renderer, verification metadata and driver receipt. Test exports are absent
// from the library and from the generated public reference.
func RLSReadFixture(table string) (tabula.Definition, tabula.SQLAuthority, func(tabula.Session[struct{}], int64) (tabula.AuthorizedStatement, error), error) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{Deny: tabula.DenyLegacySQL})
	tab := tabula.Define(reg, tabula.TableSpec[typedItem, int64]{Name: table, RelationScope: tabula.ByRLS(), ReadOnly: true})
	src := FromTable(tab, "i", tabula.ByRLS())
	out := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "entries"})
	owner := planView(out)
	c, err := newTypedCompiler(true, src.source)
	if err != nil {
		return tabula.Definition{}, tabula.SQLAuthority{}, nil, err
	}
	id := FieldOf(src, "id", Int8())
	input := Input("id", Int8())
	columns, result, err := c.project(ownerOutputLayout(owner), []Projection{Project(Output(out, "id", Int8()), id.Expr())})
	if err != nil {
		return tabula.Definition{}, tabula.SQLAuthority{}, nil, err
	}
	where, err := Eq(id.Expr(), input.Expr()).part.render(c)
	if err != nil {
		return tabula.Definition{}, tabula.SQLAuthority{}, nil, err
	}
	meta, err := c.readRequirements()
	if err != nil {
		return tabula.Definition{}, tabula.SQLAuthority{}, nil, err
	}
	meta.Result = result
	sql := "SELECT " + columns[0] + " FROM " + src.source.table.SQL() + " AS " + typedQuote(src.source.alias) + " WHERE " + where
	shape, err := c.finishShape("by_id", sql, catalogType{}, meta)
	if err != nil {
		return tabula.Definition{}, tabula.SQLAuthority{}, nil, err
	}
	if err := registerShapes(owner, "by_id", []*compiledShape{shape}); err != nil {
		return tabula.Definition{}, tabula.SQLAuthority{}, nil, err
	}
	bind := func(s tabula.Session[struct{}], value int64) (tabula.AuthorizedStatement, error) {
		return bindShapeCall(owner, shape, s, tabula.CallScan, []Binding{BindValue(input, value)}, false, false)
	}
	return tabula.Definition(out.Definition()), reg.SQLAuthority(), bind, nil
}

// ReturningRowsFixture builds the registered result stream used to exercise
// the transaction adapter before additional write builders are installed.
func ReturningRowsFixture(table string) (tabula.SQLAuthority, func(tabula.Session[struct{}]) (tabula.AuthorizedStatement, error), error) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{Deny: tabula.DenyLegacySQL})
	tab := tabula.Define(reg, tabula.TableSpec[typedItem, int64]{Name: table, RelationScope: tabula.Global()})
	src := FromTable(tab, "i", tabula.Global())
	view := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "entries"})
	owner := planView(view)
	c, err := newTypedCompiler(true, src.source)
	if err != nil {
		return tabula.SQLAuthority{}, nil, err
	}
	query := "UPDATE " + src.source.table.SQL() + " SET label = label RETURNING id"
	meta := tabula.StatementShape{Effects: tabula.EffectWrite, Access: []tabula.ShapeAccess{{Relation: src.source.table.SQL(), Command: "UPDATE"}},
		Result: []tabula.ShapeField{{Name: "id", Type: typeID(Int8()).Qualified()}}}
	shape, err := c.finishShape("touch", query, catalogType{}, meta)
	if err != nil {
		return tabula.SQLAuthority{}, nil, err
	}
	if err := registerShapes(owner, "touch", []*compiledShape{shape}); err != nil {
		return tabula.SQLAuthority{}, nil, err
	}
	return reg.SQLAuthority(), func(s tabula.Session[struct{}]) (tabula.AuthorizedStatement, error) {
		return bindShapeCall(owner, shape, s, tabula.CallReturningEach, nil, false, false)
	}, nil
}
