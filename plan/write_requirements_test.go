package plan

import (
	"testing"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/internal/planrt"
	"github.com/stretchr/testify/require"
)

func TestWriteRequirementsFreeze(t *testing.T) {
	shape := tabula.StatementShape{
		UniqueMatches:      []tabula.UniqueMatchRequirement{{Relation: "public.items", Columns: []string{"id"}}},
		Defaults:           []tabula.DefaultRequirement{{Relation: "public.notes", Columns: []string{"id"}, IncludesOmitted: true, InsertColumns: []string{"note"}}},
		Conflicts:          []tabula.ConflictRequirement{{Relation: "public.items", Columns: []string{"id"}, DoUpdate: true}},
		ReturningRelations: []string{"public.items"},
		ResultLineage:      []tabula.ResultLineage{{Relation: "public.items", Column: "id"}, {CastType: "pg_catalog.text"}},
	}
	cloned := cloneShape(shape)
	cloned.UniqueMatches[0].Columns[0] = "note"
	cloned.Defaults[0].Columns[0] = "note"
	cloned.Defaults[0].InsertColumns[0] = "id"
	cloned.Conflicts[0].Columns[0] = "note"
	cloned.ReturningRelations[0] = "public.jobs"
	cloned.ResultLineage[0].Column = "note"
	cloned.ResultLineage[1].CastType = "pg_catalog.int8"
	require.Equal(t, "id", shape.UniqueMatches[0].Columns[0])
	require.Equal(t, "id", shape.Defaults[0].Columns[0])
	require.Equal(t, "note", shape.Defaults[0].InsertColumns[0])
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
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table := tabula.DefinePlanSource(reg, tabula.TableSpec[item, int64]{Name: "public.items", RelationScope: tabula.Global(), WriteMode: tabula.WriteInsertOnly})
	src := FromTable(table, "i", tabula.Global()).source
	issuer := src.issuer
	write := statementWrite{ID: &statementEffectIdentity{}, Target: src.table, Command: "INSERT"}
	shape := &compiledShape{
		compiled: &planrt.CompiledShape{Key: "insert"},
		sources:  []*occurrence{src},
		shape: tabula.StatementShape{Effects: tabula.EffectWrite, Result: []tabula.ShapeField{{Name: "value", Type: typeID(Int8()).Qualified()}},
			Access: []tabula.ShapeAccess{{Relation: src.table.SQL(), Command: "INSERT"}}},
	}
	// An ordinary result-bearing insert remains forbidden.
	require.ErrorIs(t, shape.checkSources(issuer), tabula.ErrOperationDenied)
	shape.compiled.WriteEffects = statementEffects{Writes: []statementWrite{write}, WriteCTE: true}
	// An effect-only insert contributes no read or RETURNING on its target.
	require.NoError(t, shape.checkSources(issuer))
	shape.compiled.WriteEffects.Writes[0].Returning = true
	require.ErrorIs(t, shape.checkSources(issuer), tabula.ErrOperationDenied)
	shape.compiled.WriteEffects.Writes[0].Returning = false
	shape.shape.Access = append(shape.shape.Access, tabula.ShapeAccess{Relation: src.table.SQL(), Command: "SELECT"})
	require.ErrorIs(t, shape.checkSources(issuer), tabula.ErrOperationDenied)
}
