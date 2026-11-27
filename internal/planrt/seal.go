// Package planrt provides the sealed runtime bridge between package tabula
// and package tabula/plan. Because it lives under internal/, external packages
// cannot construct a Seal or forge statement authority.
package planrt

import "slices"

// Seal is an unforgeable token proving that the caller lives inside the
// github.com/0xJean/tabula module.
type Seal struct{ valid bool }

// Key returns a valid internal bridge seal.
func Key() Seal { return Seal{valid: true} }

// Valid reports whether s was constructed by Key.
func (s Seal) Valid() bool { return s.valid }

// Method constants match tabula.CallMethod.
const (
	MethodExec uint8 = iota + 1
	MethodScan
	MethodEach
	MethodReturning
	MethodReturningEach
)

// Effect bit constants match tabula.ShapeEffects.
const (
	EffectRead uint8 = 1 << iota
	EffectWrite
	EffectRowLock
	EffectAdvisoryLock
)

// Issuer identifies the Registry that owns and verifies compiled shapes.
type Issuer struct{ Strict bool }

// EntryID is a non-zero-sized unique identity token for one registered
// Table or View entry.
type EntryID struct{ _ byte }

// EffectIdentity is a non-zero-sized identity token for one modifying CTE or
// write declaration so independent effects never alias.
type EffectIdentity struct{ _ byte }

// WriteEffect records one physical write command inside a compiled shape.
type WriteEffect struct {
	ID        *EffectIdentity
	Target    PhysicalName
	Command   string
	Returning bool
}

// StatementEffects captures immutable write/CTE/routine effects attached to a
// compiled shape.
type StatementEffects struct {
	Bits         uint8
	Writes       []WriteEffect
	WriteCTE     bool
	WriteRoutine bool
}

// Returns reports whether any write effect targets target with RETURNING.
func (e StatementEffects) Returns(target PhysicalName) bool {
	for _, w := range e.Writes {
		if w.Target == target && w.Returning {
			return true
		}
	}
	return false
}

// Clone returns a copy with an independent Writes slice.
func (e StatementEffects) Clone() StatementEffects {
	e.Writes = slices.Clone(e.Writes)
	return e
}

// RoutineMeta records one routine's signature and ReadOnly flag for
// StatementGuard verification of write routines.
type RoutineMeta struct {
	Signature string
	ReadOnly  bool
}

// FunctionGrant records one function EXECUTE privilege on a compiled shape.
type FunctionGrant struct {
	Function  string
	Privilege string
}

// CompiledShape is the immutable driver-verified execution descriptor shared
// between tabula's SQLAuthority / StatementGuard and compiled statement plans.
type CompiledShape struct {
	ObservationKey string
	TableName      string
	Action         string
	FixedMethod    uint8
	Authority      *Issuer
	Key            string
	SQL            string
	Effects        uint8
	HasResult      bool
	UnscopedOnly   bool
	WriteEffects   StatementEffects
	Routines       []RoutineMeta
	FunctionGrants []FunctionGrant
}

// PermitsMethod reports whether this shape authorizes driver method.
func (s *CompiledShape) PermitsMethod(method uint8) bool {
	if s.FixedMethod != 0 {
		return s.FixedMethod == method
	}
	write := s.Effects&EffectWrite != 0
	rows := s.HasResult
	switch method {
	case MethodExec:
		return write && !rows
	case MethodScan, MethodEach:
		return !write && rows
	case MethodReturning, MethodReturningEach:
		return write && rows
	default:
		return false
	}
}
