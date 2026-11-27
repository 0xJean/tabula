package plan

import (
	"slices"

	"github.com/0xJean/tabula"
)

// WriteStatement is a sealed declaration of a mutation. It does not execute SQL.
// A write plan checks its physical targets and registers all permitted forms.
type WriteStatement interface{ writeNode() statementNode }

// ResultStatement is a sealed declaration of a statement producing rows.
// A returning plan requires an actual write effect, even when its outer command
// is SELECT. A read declaration cannot obtain write authority through this type.
type ResultStatement interface{ resultNode() statementNode }

// The source walk precedes rendering: every block shares the final parameter
// layout, including a possible scope slot. No child can silently allocate its
// own $1 or omit an unreferenced modifying common table from its effect record.
type statementNode interface {
	statementSources() ([]rowSource, error)
	renderStatement(*statementContext) (statementResult, error)
}

type statementContext struct {
	compiler *typedCompiler
	block    *queryBlock
	position statementPosition
	// A nil output denotes an intermediate result without a Go decoder.
	output  *outputLayout
	indexes []int
}

type statementPosition uint8

const (
	statementRoot statementPosition = iota
	statementDerived
	statementScalar
	statementSetArm
	statementCommonTable
	statementRecursiveStep
)

type statementResult struct {
	sql     string
	fields  []queryResultField
	keys    []queryResultKey
	shape   tabula.StatementShape
	effects statementEffects
}

// Identity tokens are deliberately nonzero-sized. Two independent exports or
// effect declarations must never compare equal because of zero-sized addresses.
type queryFieldIdentity struct{ _ byte }

type queryResultField struct {
	id      *queryFieldIdentity
	name    string
	value   scalarOperand
	secret  bool
	origins []physicalColumn
	// Logical result evidence is separate from expression dependencies. A cast
	// cannot simultaneously claim the physical origin of its operand.
	lineage tabula.ResultLineage
	// Stored values may be read twice within one result row. Stability still
	// records their provenance and is not upgraded by materialization.
	stored    bool
	stability expressionStability
	proof     queryValueProof
}

type queryResultKey struct {
	fields      []*queryFieldIdentity
	keys        []tabula.KeyRequirement
	comparisons []tabula.ComparisonRequirement
}

func cloneQueryFields(fields []queryResultField) []queryResultField {
	out := slices.Clone(fields)
	for i := range out {
		out[i].origins = slices.Clone(out[i].origins)
		out[i].proof = out[i].proof.frozen()
	}
	return out
}

func cloneQueryKeys(keys []queryResultKey) []queryResultKey {
	out := slices.Clone(keys)
	for i := range out {
		out[i].fields = slices.Clone(out[i].fields)
		out[i].keys = appendKeyRequirements(nil, out[i].keys...)
		out[i].comparisons = slices.Clone(out[i].comparisons)
	}
	return out
}

func (r statementResult) frozen() statementResult {
	r.fields = cloneQueryFields(r.fields)
	r.keys = cloneQueryKeys(r.keys)
	r.shape = cloneShape(r.shape)
	r.effects = r.effects.Clone()
	return r
}
