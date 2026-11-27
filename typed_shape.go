package tabula

import (
	"maps"
	"slices"
)

// ShapeEffects describes what a compiled statement can do on its session.
type ShapeEffects uint8

const (
	// EffectRead marks a statement that observes rows.
	EffectRead ShapeEffects = 1 << iota
	// EffectWrite marks a statement that can modify data, including through a routine.
	EffectWrite
	// EffectRowLock marks a row-locking read that needs a writable transaction.
	EffectRowLock
	// EffectAdvisoryLock marks an operation whose advisory lock needs a transaction.
	EffectAdvisoryLock
)

// ShapeField describes a result column in projection order.
// When AllowedTypes is non-empty, sqlcheck accepts any PostgreSQL type whose
// exact or unwrapped domain base OID matches one of the listed type names.
type ShapeField struct {
	Name, Type   string
	Nullable     bool
	AllowedTypes []string `json:",omitempty"`
}

// ShapeInput identifies a numbered parameter. Scope marks a parameter supplied
// by the session. Values are kept outside all snapshots.
// When AllowedTypes is non-empty, sqlcheck accepts any PostgreSQL parameter
// type whose exact or unwrapped domain base OID matches one of the listed names.
type ShapeInput struct {
	Name, Type string
	Scope      bool
	// Secret requests redaction of this bind value by observers.
	Secret       bool
	AllowedTypes []string `json:",omitempty"`
}

// ShapeAccess identifies one physical relation and command for policy checks.
type ShapeAccess struct{ Relation, Command string }

// ShapeColumn records the declared PostgreSQL type of a referenced physical
// column for catalog verification. Relation is its quoted, qualified table.
type ShapeColumn struct {
	Relation, Name, Type string
	// TypeMod is the expected pg_attribute.atttypmod when the declaration
	// specifies a modifier. Nil requests verification of the type OID alone.
	TypeMod *int32 `json:",omitempty"`
}

// RoutineRequirement describes one exact, non-variadic pg_proc entry.
// Signature and ResultType use schema-qualified catalog names. ReadOnly
// requires STABLE or IMMUTABLE. SecurityDefiner must match the function.
type RoutineRequirement struct {
	Signature, ResultType                 string
	ReturnsSet, ReadOnly, SecurityDefiner bool
}

// KeyRequirement records a complete immediate primary key used to prove a
// result order. Relation is a quoted, qualified scanned relation, including
// its descendants. Catalog verification rejects ordinary inheritance and
// accepts a valid parent-wide primary key on a partitioned relation.
type KeyRequirement struct {
	Relation string
	Columns  []string
}

// ComparisonRequirement requires native comparison semantics and a
// deterministic collation for a physical column used by a result order.
// When WithRelation and WithColumn are set, both columns also require
// compatible implicit collations within one expression.
type ComparisonRequirement struct {
	Relation, Column         string
	WithRelation, WithColumn string `json:",omitempty"`
}

// StatementShape describes the inputs, results, effects and minimum grants for
// one SQL string in Definition.Statements under the same key.
type StatementShape struct {
	Inputs          []ShapeInput
	Result          []ShapeField
	Effects         ShapeEffects
	Privileges      []DefinedPrivilege
	Access          []ShapeAccess
	PhysicalColumns []ShapeColumn
	ForceRLS        []string
	SchemaUsage     []string
	AnyColumnSelect []string
	AnyColumnUpdate []string
	// Routines records the catalog contracts of called PostgreSQL functions.
	Routines []RoutineRequirement
	// Keys records the catalog primary-key premises of deterministic results.
	Keys []KeyRequirement `json:",omitempty"`
	// Comparisons records native ordering and deterministic collation premises.
	Comparisons []ComparisonRequirement `json:",omitempty"`
	// UniqueMatches proves at most one matching write-source row.
	UniqueMatches []UniqueMatchRequirement `json:",omitempty"`
	// Defaults records columns whose catalog defaults supply write values.
	Defaults []DefaultRequirement `json:",omitempty"`
	// Conflicts records premises checked against actual EXPLAIN arbiters.
	Conflicts []ConflictRequirement `json:",omitempty"`
	// ReturningRelations identifies physical targets with a RETURNING clause.
	ReturningRelations []string `json:",omitempty"`
	// ResultLineage supplies logical type evidence in Result order. An empty
	// entry supplies no additional evidence beyond the server description.
	ResultLineage []ResultLineage `json:",omitempty"`
	// UnscopedOnly marks a shape that omits a declared column-scope predicate.
	// It can be bound only to an unscoped session.
	UnscopedOnly bool
	// StructSQL marks a named-parameter SQL declaration registered via DefineSQL,
	// DefineSQLExec, DefineSQLReturning or DefineSQLList.
	StructSQL bool `json:",omitempty"`
}

func appendKeyRequirements(dst []KeyRequirement, values ...KeyRequirement) []KeyRequirement {
	for _, v := range values {
		if !slices.ContainsFunc(dst, func(k KeyRequirement) bool { return k.Relation == v.Relation && slices.Equal(k.Columns, v.Columns) }) {
			v.Columns = slices.Clone(v.Columns)
			dst = append(dst, v)
		}
	}
	return dst
}

func cloneShape(s StatementShape) StatementShape {
	s.Inputs = slices.Clone(s.Inputs)
	for i := range s.Inputs {
		s.Inputs[i].AllowedTypes = slices.Clone(s.Inputs[i].AllowedTypes)
	}
	s.Result = slices.Clone(s.Result)
	for i := range s.Result {
		s.Result[i].AllowedTypes = slices.Clone(s.Result[i].AllowedTypes)
	}
	s.Privileges = slices.Clone(s.Privileges)
	for i := range s.Privileges {
		s.Privileges[i].Columns = slices.Clone(s.Privileges[i].Columns)
	}
	s.Access = slices.Clone(s.Access)
	s.PhysicalColumns = slices.Clone(s.PhysicalColumns)
	for i, col := range s.PhysicalColumns {
		if col.TypeMod != nil {
			mod := *col.TypeMod
			s.PhysicalColumns[i].TypeMod = &mod
		}
	}
	s.ForceRLS = slices.Clone(s.ForceRLS)
	s.SchemaUsage = slices.Clone(s.SchemaUsage)
	s.AnyColumnSelect = slices.Clone(s.AnyColumnSelect)
	s.AnyColumnUpdate = slices.Clone(s.AnyColumnUpdate)
	s.Routines = slices.Clone(s.Routines)
	s.Keys = appendKeyRequirements(nil, s.Keys...)
	s.Comparisons = slices.Clone(s.Comparisons)
	cloneWriteRequirements(&s)
	return s
}

func cloneShapeMap(m map[string]StatementShape) map[string]StatementShape {
	out := maps.Clone(m)
	for key, s := range out {
		out[key] = cloneShape(s)
	}
	return out
}
