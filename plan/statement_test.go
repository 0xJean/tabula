package plan

import (
	"github.com/0xJean/tabula"

	"testing"

	"github.com/stretchr/testify/require"
)

type contractStatement struct {
	sources []rowSource
	result  statementResult
}

func (n contractStatement) statementSources() ([]rowSource, error) { return n.sources, nil }
func (n contractStatement) renderStatement(c *statementContext) (statementResult, error) {
	if c.output != nil && len(c.indexes) > 0 {
		return n.result.frozen(), c.block.resolvePhysical(n.sources[0].rel, 0)
	}
	_, err := c.compiler.params.parameter(&typedInput{name: "value", kind: typeID(Int8())})
	return n.result.frozen(), err
}

func TestQueryScopeIdentity(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	tab := tabula.DefinePlanSource(reg, tabula.TableSpec[typedItem, int64]{Name: "public.items", RelationScope: tabula.ByRLS()})
	r := FromTable(tab, "i", tabula.ByRLS())
	issuer := r.source.issuer
	root, err := newQueryBlock(nil, issuer)
	require.NoError(t, err)
	require.NoError(t, root.addPhysical(r.source))
	require.Error(t, root.addPhysical(FromTable(tab, "i", tabula.ByRLS()).source))
	child, err := newQueryBlock(root, issuer)
	require.NoError(t, err)
	inner := FromTable(tab, "i", tabula.ByRLS())
	require.NoError(t, child.addPhysical(inner.source))
	require.Error(t, child.resolvePhysical(r.source, 0))
	require.NoError(t, child.resolvePhysical(r.source, 1))
	require.Error(t, child.resolvePhysical(inner.source, 1))
	require.Error(t, child.resolvePhysical(r.source, 2))
	require.Error(t, child.resolvePhysical(r.source, -1))
	require.Error(t, child.resolvePhysical(nil, 0))
	sibling, err := newQueryBlock(root, issuer)
	require.NoError(t, err)
	require.Error(t, sibling.resolvePhysical(inner.source, 0))
	foreign := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	foreignTab := tabula.DefinePlanSource(foreign, tabula.TableSpec[typedItem, int64]{Name: "public.items", RelationScope: tabula.ByRLS()})
	foreignIssuer := planTable(foreignTab).Issuer
	_, err = newQueryBlock(root, foreignIssuer)
	require.Error(t, err)
	_, err = newQueryBlock(nil, nil)
	require.Error(t, err)
	require.Error(t, root.addPhysical(&occurrence{issuer: foreignIssuer}))
	require.Error(t, root.addPhysical(nil))
	require.Error(t, (*queryBlock)(nil).addPhysical(r.source))

	id := &queryFieldIdentity{}
	source := &queryOccurrence{alias: "d", node: contractStatement{}, fields: []queryResultField{{
		id: id, name: "id", value: FieldOf(r, "id", Int8()).Expr().scalarOperand(),
		origins: []physicalColumn{{Table: r.source.table, Name: "id"}}, stored: true, stability: expressionVolatile,
	}}}
	require.NoError(t, root.addResult(source))
	_, err = child.resolveResult(source, id, 0)
	require.Error(t, err)
	field, err := child.resolveResult(source, id, 1)
	require.NoError(t, err)
	require.True(t, field.stored)
	require.Equal(t, expressionVolatile, field.stability)
	require.Equal(t, typeID(Int8()), field.value.kind)
	field.origins[0].Name = "changed"
	require.Equal(t, "id", source.fields[0].origins[0].Name)
	_, err = child.resolveResult(source, &queryFieldIdentity{}, 1)
	require.Error(t, err)
	_, err = child.resolveResult(source, nil, 1)
	require.Error(t, err)
	_, err = child.resolveResult(source, id, 2)
	require.Error(t, err)
	require.Error(t, root.addResult(source))
	require.Error(t, root.addResult(nil))
	require.Error(t, root.addResult(&queryOccurrence{alias: "d"}))
	require.Error(t, (*queryBlock)(nil).addResult(source))
}

func TestStatementEffectUnion(t *testing.T) {
	write := statementWrite{ID: &statementEffectIdentity{}, Target: physicalName{Schema: "public", Name: "items"}, Command: "UPDATE"}
	first := statementEffects{Bits: uint8(tabula.EffectRead), Writes: []statementWrite{write}, WriteCTE: true}
	var empty statementEffects
	combined, err := combineStatementEffects(empty, first)
	require.NoError(t, err)
	require.Equal(t, uint8(tabula.EffectRead|tabula.EffectWrite), combined.Bits)
	combined, err = combineStatementEffects(combined, first)
	require.NoError(t, err)
	require.Len(t, combined.Writes, 1)
	second := statementEffects{Writes: []statementWrite{{ID: &statementEffectIdentity{}, Target: physicalName{Schema: "public", Name: "notes"}, Command: "INSERT"}}}
	combined, err = combineStatementEffects(combined, second)
	require.NoError(t, err)
	require.Len(t, combined.Writes, 2)
	require.True(t, combined.WriteCTE)
	combined.Writes[0].Command = "DELETE"
	require.Equal(t, "UPDATE", first.Writes[0].Command)

	for _, invalid := range []statementWrite{
		{ID: &statementEffectIdentity{}, Target: write.Target, Command: "DELETE"},
		{ID: write.ID, Target: write.Target, Command: "UPDATE", Returning: true},
		{ID: write.ID, Target: physicalName{Schema: "public", Name: "jobs"}, Command: "UPDATE"},
		{Target: write.Target, Command: "UPDATE"},
		{ID: &statementEffectIdentity{}, Target: write.Target, Command: "SELECT"},
		{ID: &statementEffectIdentity{}, Command: "INSERT"},
	} {
		_, err := combineStatementEffects(first, statementEffects{Writes: []statementWrite{invalid}})
		require.Error(t, err)
		require.Len(t, first.Writes, 1)
		require.Equal(t, "UPDATE", first.Writes[0].Command)
	}
	upsert := write
	upsert.Command = "INSERT"
	combined, err = combineStatementEffects(first, statementEffects{Writes: []statementWrite{upsert}})
	require.NoError(t, err)
	require.Len(t, combined.Writes, 2)
	combined, err = combineStatementEffects(combined, statementEffects{Writes: []statementWrite{upsert}})
	require.NoError(t, err)
	require.Len(t, combined.Writes, 2)
	require.False(t, combined.Returns(write.Target))
	combined.Writes[0].Returning = true
	require.True(t, combined.Returns(write.Target))
	require.False(t, combined.Returns(physicalName{Schema: "public", Name: "jobs"}))
	routine, err := combineStatementEffects(empty, statementEffects{WriteRoutine: true})
	require.NoError(t, err)
	require.Equal(t, uint8(tabula.EffectWrite), routine.Bits)
	require.True(t, routine.WriteRoutine)
}

func TestStatementResultFreeze(t *testing.T) {
	field := &queryFieldIdentity{}
	result := statementResult{
		sql: "SELECT 1", fields: []queryResultField{{id: field, name: "id", secret: true}},
		keys: []queryResultKey{{fields: []*queryFieldIdentity{field}, keys: []tabula.KeyRequirement{{Relation: "public.items", Columns: []string{"id"}}},
			comparisons: []tabula.ComparisonRequirement{{Relation: "public.items", Column: "id"}}}},
		shape: tabula.StatementShape{Effects: tabula.EffectRead, Result: []tabula.ShapeField{{Name: "id", Type: "int8"}}},
	}
	node := contractStatement{result: result}
	compiler, err := newTypedCompiler(false)
	require.NoError(t, err)
	for _, position := range []statementPosition{statementRoot, statementDerived, statementScalar, statementSetArm, statementCommonTable, statementRecursiveStep} {
		c := &statementContext{compiler: compiler, position: position}
		require.Equal(t, position, c.position)
		frozen, err := node.renderStatement(c)
		require.NoError(t, err)
		require.Equal(t, "SELECT 1", frozen.sql)
		require.True(t, frozen.fields[0].secret)
		frozen.fields[0].name = "changed"
		frozen.keys[0].fields[0] = &queryFieldIdentity{}
		frozen.keys[0].keys[0].Columns[0] = "changed"
		frozen.keys[0].comparisons[0].Column = "changed"
		frozen.shape.Result[0].Name = "changed"
		require.Equal(t, "id", result.fields[0].name)
		require.Same(t, field, result.keys[0].fields[0])
		require.Equal(t, "id", result.keys[0].keys[0].Columns[0])
		require.Equal(t, "id", result.keys[0].comparisons[0].Column)
		require.Equal(t, "id", result.shape.Result[0].Name)
		compiler.params = newParameterLayout(false)
	}
	sources, err := node.statementSources()
	require.NoError(t, err)
	require.Empty(t, sources)
}
