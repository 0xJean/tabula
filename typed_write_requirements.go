package tabula

import "slices"

// UniqueMatchRequirement records a physical write-source uniqueness premise.
// Columns must contain a complete immediate, valid unique index key whose
// columns are NOT NULL.
// A table name or an application assertion is not evidence of uniqueness.
type UniqueMatchRequirement struct {
	Relation string
	Columns  []string
}

// DefaultRequirement records columns whose database defaults are used by a
// write. Verification distinguishes identity generation from sequence defaults
// and does not infer arbitrary default-function privileges from SQL text.
type DefaultRequirement struct {
	Relation string
	Columns  []string // explicitly selected DEFAULT columns
	// IncludesOmitted adds all physical columns absent from InsertColumns.
	// Only INSERT declarations set it; an empty InsertColumns means DEFAULT VALUES.
	IncludesOmitted bool     `json:",omitzero"`
	InsertColumns   []string `json:",omitempty"`
}

// ConflictRequirement records the target of a native conflict-inference clause.
// Constraint and Columns are alternative targets. The original SQL and its
// EXPLAIN arbiter indexes supply the inference evidence, including a partial
// index predicate. DoUpdate requires a nondeferrable unique arbiter.
type ConflictRequirement struct {
	Relation, Constraint string
	Columns              []string
	DoUpdate             bool
}

// ResultLineage records evidence for a result's logical SQL type. Relation and
// Column identify an exact physical origin; CastType identifies an explicit
// outer result conversion. These alternatives require independent agreement
// with the server description/catalog or the original result expression.
// A declaration alone, or a matching wire base OID, cannot prove a domain.
type ResultLineage struct {
	Relation, Column, CastType string
}

func cloneWriteRequirements(s *StatementShape) {
	s.UniqueMatches = slices.Clone(s.UniqueMatches)
	for i := range s.UniqueMatches {
		s.UniqueMatches[i].Columns = slices.Clone(s.UniqueMatches[i].Columns)
	}
	s.Defaults = slices.Clone(s.Defaults)
	for i := range s.Defaults {
		s.Defaults[i].Columns = slices.Clone(s.Defaults[i].Columns)
		s.Defaults[i].InsertColumns = slices.Clone(s.Defaults[i].InsertColumns)
	}
	s.Conflicts = slices.Clone(s.Conflicts)
	for i := range s.Conflicts {
		s.Conflicts[i].Columns = slices.Clone(s.Conflicts[i].Columns)
	}
	s.ReturningRelations = slices.Clone(s.ReturningRelations)
	s.ResultLineage = slices.Clone(s.ResultLineage)
}
