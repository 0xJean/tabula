package tabula

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteRequirementsFreeze(t *testing.T) {
	shape := StatementShape{
		UniqueMatches:      []UniqueMatchRequirement{{Relation: "public.items", Columns: []string{"id"}}},
		Defaults:           []DefaultRequirement{{Relation: "public.notes", Columns: []string{"id"}}},
		Conflicts:          []ConflictRequirement{{Relation: "public.items", Columns: []string{"id"}, DoUpdate: true}},
		ReturningRelations: []string{"public.items"},
		ResultLineage:      []ResultLineage{{Relation: "public.items", Column: "id"}, {CastType: "pg_catalog.text"}},
	}
	cloned := cloneShape(shape)
	cloned.UniqueMatches[0].Columns[0] = "note"
	cloned.Defaults[0].Columns[0] = "note"
	cloned.Conflicts[0].Columns[0] = "note"
	cloned.ReturningRelations[0] = "public.jobs"
	cloned.ResultLineage[0].Column = "note"
	cloned.ResultLineage[1].CastType = "pg_catalog.int8"
	require.Equal(t, "id", shape.UniqueMatches[0].Columns[0])
	require.Equal(t, "id", shape.Defaults[0].Columns[0])
	require.Equal(t, "id", shape.Conflicts[0].Columns[0])
	require.Equal(t, "public.items", shape.ReturningRelations[0])
	require.Equal(t, "id", shape.ResultLineage[0].Column)
	require.Equal(t, "pg_catalog.text", shape.ResultLineage[1].CastType)
}

func TestInsertOnlyEffectResultBoundary(t *testing.T) {
	type item struct {
		ID   int64  `db:"id,key"`
		Note string `db:"note"`
	}
	reg := NewRegistry(RegistryConfig[struct{}]{})
	table := DefinePlanSource(reg, TableSpec[item, int64]{Name: "public.items", RelationScope: Global(), WriteMode: WriteInsertOnly})
	src := FromTable(table, "i", Global()).source
	write := statementWrite{id: &statementEffectIdentity{}, target: src.table, command: "INSERT"}
	shape := &compiledShape{
		sources: []*occurrence{src},
		shape: StatementShape{Effects: EffectWrite, Result: []ShapeField{{Name: "value", Type: Int8().id.qualified()}},
			Access: []ShapeAccess{{Relation: src.table.sql(), Command: "INSERT"}}},
	}
	// An ordinary result-bearing insert remains forbidden.
	require.ErrorIs(t, shape.checkSources(reg.statementIssuer), ErrOperationDenied)
	shape.effects = statementEffects{writes: []statementWrite{write}, writeCTE: true}
	// An effect-only insert contributes no read or RETURNING on its target.
	require.NoError(t, shape.checkSources(reg.statementIssuer))
	shape.effects.writes[0].returning = true
	require.ErrorIs(t, shape.checkSources(reg.statementIssuer), ErrOperationDenied)
	shape.effects.writes[0].returning = false
	shape.shape.Access = append(shape.shape.Access, ShapeAccess{Relation: src.table.sql(), Command: "SELECT"})
	require.ErrorIs(t, shape.checkSources(reg.statementIssuer), ErrOperationDenied)
}
