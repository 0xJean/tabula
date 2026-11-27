//go:build integration

package plan

import "github.com/0xJean/tabula"

// SignedPolicyFixture exercises the driver policy channel without depending on
// the graph-builder package. It is absent from production builds and GoDoc.
func SignedPolicyFixture(name string) (tabula.SQLAuthority, func(tabula.Session[struct{}], int64) (tabula.AuthorizedStatement, error), error) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table := tabula.DefinePlanSource(reg, tabula.TableSpec[typedItem, int64]{Name: name, RelationScope: tabula.Global()})
	rel := FromTable(table, "i", tabula.Global())
	out := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "entries"})
	owner := planView(out)
	c, err := newTypedCompiler(true, rel.source)
	if err != nil {
		return tabula.SQLAuthority{}, nil, err
	}
	input := Input("id", Int8())
	value, err := input.Expr().part.render(c)
	if err != nil {
		return tabula.SQLAuthority{}, nil, err
	}
	query := "WITH added AS (INSERT INTO " + rel.source.table.SQL() + " (id) VALUES (" + value + ") RETURNING id) SELECT id FROM added"
	meta := tabula.StatementShape{Effects: tabula.EffectWrite, Result: []tabula.ShapeField{{Name: "id", Type: typeID(Int8()).Qualified()}},
		Access: []tabula.ShapeAccess{{Relation: rel.source.table.SQL(), Command: "INSERT"}}}
	shape, err := c.finishShape("append", query, catalogType{}, meta)
	if err != nil {
		return tabula.SQLAuthority{}, nil, err
	}
	shape.compiled.WriteEffects = statementEffects{Bits: uint8(tabula.EffectWrite), WriteCTE: true, Writes: []statementWrite{{ID: &statementEffectIdentity{}, Target: rel.source.table, Command: "INSERT", Returning: true}}}
	if err := registerShapes(owner, "append", []*compiledShape{shape}); err != nil {
		return tabula.SQLAuthority{}, nil, err
	}
	return reg.SQLAuthority(), func(s tabula.Session[struct{}], id int64) (tabula.AuthorizedStatement, error) {
		return bindShapeCall(owner, shape, s, tabula.CallReturningEach, []Binding{BindValue(input, id)}, false, false)
	}, nil
}
