package tabula

// Capability is a bitmask of operations that RegistryConfig.Deny forbids on
// tables and bindings belonging to a Registry.
type Capability uint32

const (
	// DenyDelete rejects TableSpec.AllowDelete at Define/TryDefine time.
	DenyDelete Capability = 1 << iota
	// DenyUpsert rejects Table.DefineUpsert / TryDefineUpsert.
	DenyUpsert
	// DenyUnscopedWrites rejects derived writes, hand-written Rows.Exec and
	// binary COPY on unscoped sessions at runtime.
	DenyUnscopedWrites
	// DenyCopy rejects binary COPY operations checked via Rows.CheckCopy.
	DenyCopy
)
