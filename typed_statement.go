package tabula

import (
	"fmt"
	"slices"
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
	output  *registration
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
	shape   StatementShape
	effects statementEffects
}

// Identity tokens are deliberately nonzero-sized. Two independent exports or
// effect declarations must never compare equal because of zero-sized addresses.
type queryFieldIdentity struct{ _ byte }
type statementEffectIdentity struct{ _ byte }

type queryResultField struct {
	id      *queryFieldIdentity
	name    string
	value   scalarOperand
	secret  bool
	origins []physicalColumn
	// Stored values may be read twice within one result row. Stability still
	// records their provenance and is not upgraded by materialization.
	stored    bool
	stability expressionStability
}

type queryResultKey struct {
	fields      []*queryFieldIdentity
	keys        []KeyRequirement
	comparisons []ComparisonRequirement
}

func cloneQueryFields(fields []queryResultField) []queryResultField {
	out := slices.Clone(fields)
	for i := range out {
		out[i].origins = slices.Clone(out[i].origins)
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

type statementWrite struct {
	id        *statementEffectIdentity
	target    physicalName
	command   string
	returning bool
}

type statementEffects struct {
	bits         ShapeEffects
	writes       []statementWrite
	writeCTE     bool
	writeRoutine bool
}

// combine is atomic on failure and counts a reused common-table declaration
// once. Independent effects on the same physical target are rejected even when
// they use different aliases or are not referenced by the final projection.
func (e statementEffects) combine(other statementEffects) (statementEffects, error) {
	out := e
	out.writes = slices.Clone(e.writes)
	out.bits |= other.bits
	out.writeCTE = e.writeCTE || other.writeCTE
	out.writeRoutine = e.writeRoutine || other.writeRoutine
	for _, next := range other.writes {
		if next.id == nil || !typedIdent(next.target.schema) || !typedIdent(next.target.name) {
			return statementEffects{}, fmt.Errorf("tabula: incomplete physical write effect")
		}
		switch next.command {
		case "INSERT", "UPDATE", "DELETE":
		default:
			return statementEffects{}, fmt.Errorf("tabula: unsupported physical write command %q", next.command)
		}
		reused := false
		for _, prior := range out.writes {
			if prior.id == next.id {
				if prior.target != next.target || prior.returning != next.returning {
					return statementEffects{}, fmt.Errorf("tabula: inconsistent reused write effect")
				}
				if prior.command == next.command {
					reused = true
					break
				}
				continue
			}
			if prior.target == next.target {
				return statementEffects{}, fmt.Errorf("tabula: multiple writes target %s in one statement", next.target.sql())
			}
		}
		if !reused {
			out.writes = append(out.writes, next)
		}
	}
	if len(out.writes) > 0 || out.writeCTE || out.writeRoutine {
		out.bits |= EffectWrite
	}
	return out, nil
}

func (e statementEffects) returns(target physicalName) bool {
	for _, write := range e.writes {
		if write.target == target && write.returning {
			return true
		}
	}
	return false
}

func (r statementResult) frozen() statementResult {
	r.fields = cloneQueryFields(r.fields)
	r.keys = cloneQueryKeys(r.keys)
	r.shape = cloneShape(r.shape)
	r.effects.writes = slices.Clone(r.effects.writes)
	return r
}
