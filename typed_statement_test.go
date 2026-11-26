package tabula

import (
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
	_, err := c.compiler.params.parameter(&typedInput{name: "value", kind: Int8().id})
	return n.result.frozen(), err
}

func TestQueryScopeIdentity(t *testing.T) {
	reg := NewRegistry(RegistryConfig[struct{}]{})
	tab := DefinePlanSource(reg, TableSpec[typedItem, int64]{Name: "public.items", RelationScope: ByRLS()})
	r := FromTable(tab, "i", ByRLS())
	root, err := newQueryBlock(nil, reg.statementIssuer)
	require.NoError(t, err)
	require.NoError(t, root.addPhysical(r.source))
	require.Error(t, root.addPhysical(FromTable(tab, "i", ByRLS()).source))
	child, err := newQueryBlock(root, reg.statementIssuer)
	require.NoError(t, err)
	inner := FromTable(tab, "i", ByRLS())
	require.NoError(t, child.addPhysical(inner.source))
	require.Error(t, child.resolvePhysical(r.source, 0))
	require.NoError(t, child.resolvePhysical(r.source, 1))
	require.Error(t, child.resolvePhysical(inner.source, 1))
	require.Error(t, child.resolvePhysical(r.source, 2))
	require.Error(t, child.resolvePhysical(r.source, -1))
	require.Error(t, child.resolvePhysical(nil, 0))
	sibling, err := newQueryBlock(root, reg.statementIssuer)
	require.NoError(t, err)
	require.Error(t, sibling.resolvePhysical(inner.source, 0))
	foreign := NewRegistry(RegistryConfig[struct{}]{})
	_, err = newQueryBlock(root, foreign.statementIssuer)
	require.Error(t, err)
	_, err = newQueryBlock(nil, nil)
	require.Error(t, err)
	require.Error(t, root.addPhysical(&occurrence{issuer: foreign.statementIssuer}))
	require.Error(t, root.addPhysical(nil))
	require.Error(t, (*queryBlock)(nil).addPhysical(r.source))

	id := &queryFieldIdentity{}
	source := &queryOccurrence{alias: "d", node: contractStatement{}, fields: []queryResultField{{
		id: id, name: "id", value: FieldOf(r, "id", Int8()).Expr().scalarOperand(),
		origins: []physicalColumn{{r.source.table, "id"}}, stored: true, stability: expressionVolatile,
	}}}
	require.NoError(t, root.addResult(source))
	_, err = child.resolveResult(source, id, 0)
	require.Error(t, err)
	field, err := child.resolveResult(source, id, 1)
	require.NoError(t, err)
	require.True(t, field.stored)
	require.Equal(t, expressionVolatile, field.stability)
	require.Equal(t, Int8().id, field.value.kind)
	field.origins[0].name = "changed"
	require.Equal(t, "id", source.fields[0].origins[0].name)
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
	write := statementWrite{&statementEffectIdentity{}, physicalName{"public", "items"}, "UPDATE"}
	first := statementEffects{bits: EffectRead, writes: []statementWrite{write}, writeCTE: true}
	var empty statementEffects
	combined, err := empty.combine(first)
	require.NoError(t, err)
	require.Equal(t, EffectRead|EffectWrite, combined.bits)
	combined, err = combined.combine(first)
	require.NoError(t, err)
	require.Len(t, combined.writes, 1)
	second := statementEffects{writes: []statementWrite{{&statementEffectIdentity{}, physicalName{"public", "notes"}, "INSERT"}}}
	combined, err = combined.combine(second)
	require.NoError(t, err)
	require.Len(t, combined.writes, 2)
	require.True(t, combined.writeCTE)
	combined.writes[0].command = "DELETE"
	require.Equal(t, "UPDATE", first.writes[0].command)

	for _, invalid := range []statementWrite{
		{&statementEffectIdentity{}, write.target, "DELETE"},
		{write.id, write.target, "DELETE"},
		{write.id, physicalName{"public", "jobs"}, "UPDATE"},
		{nil, write.target, "UPDATE"},
		{&statementEffectIdentity{}, write.target, "SELECT"},
		{&statementEffectIdentity{}, physicalName{}, "INSERT"},
	} {
		_, err := first.combine(statementEffects{writes: []statementWrite{invalid}})
		require.Error(t, err)
		require.Len(t, first.writes, 1)
		require.Equal(t, "UPDATE", first.writes[0].command)
	}
	routine, err := empty.combine(statementEffects{writeRoutine: true})
	require.NoError(t, err)
	require.Equal(t, EffectWrite, routine.bits)
	require.True(t, routine.writeRoutine)
}

func TestStatementResultFreeze(t *testing.T) {
	field := &queryFieldIdentity{}
	result := statementResult{
		sql: "SELECT 1", fields: []queryResultField{{id: field, name: "id", secret: true}},
		keys: []queryResultKey{{fields: []*queryFieldIdentity{field}, keys: []KeyRequirement{{Relation: "public.items", Columns: []string{"id"}}},
			comparisons: []ComparisonRequirement{{Relation: "public.items", Column: "id"}}}},
		shape: StatementShape{Effects: EffectRead, Result: []ShapeField{{Name: "id", Type: "int8"}}},
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
