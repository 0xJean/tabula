//go:build integration

package tabula

// SignedPolicyFixture exercises the driver policy channel without depending on
// the graph-builder package. It is absent from production builds and GoDoc.
func SignedPolicyFixture(name string) (SQLAuthority, func(Session[struct{}], int64) (AuthorizedStatement, error), error) {
	reg := NewRegistry(RegistryConfig[struct{}]{})
	table := DefinePlanSource(reg, TableSpec[typedItem, int64]{Name: name, RelationScope: Global()})
	rel := FromTable(table, "i", Global())
	out := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	c, err := newTypedCompiler(true, rel.source)
	if err != nil {
		return SQLAuthority{}, nil, err
	}
	input := Input("id", Int8())
	value, err := input.Expr().part.render(c)
	if err != nil {
		return SQLAuthority{}, nil, err
	}
	query := "WITH added AS (INSERT INTO " + rel.source.table.sql() + " (id) VALUES (" + value + ") RETURNING id) SELECT id FROM added"
	meta := StatementShape{Effects: EffectWrite, Result: []ShapeField{{Name: "id", Type: Int8().id.qualified()}},
		Access: []ShapeAccess{{Relation: rel.source.table.sql(), Command: "INSERT"}}}
	shape, err := c.finishShape("append", query, catalogType{}, meta)
	if err != nil {
		return SQLAuthority{}, nil, err
	}
	shape.effects = statementEffects{bits: EffectWrite, writeCTE: true, writes: []statementWrite{{id: &statementEffectIdentity{}, target: rel.source.table, command: "INSERT", returning: true}}}
	if err := out.registerShapes("append", []*compiledShape{shape}); err != nil {
		return SQLAuthority{}, nil, err
	}
	return reg.SQLAuthority(), func(s Session[struct{}], id int64) (AuthorizedStatement, error) {
		return reg.bindCall(shape, s, CallReturningEach, []Binding{BindValue(input, id)})
	}, nil
}
