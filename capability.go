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
	// DenyLegacySQL requires registered descriptor calls and forbids SQL
	// fragments, raw session methods and raw transaction access.
	DenyLegacySQL
)

type legacySQLDenial string

func (e legacySQLDenial) Error() string { return string(e) }
func (legacySQLDenial) Unwrap() error   { return ErrCapabilityDenied }

// ErrLegacySQL means the operation needs hand-written SQL or a shape that
// has not been registered. It also matches ErrCapabilityDenied.
const ErrLegacySQL legacySQLDenial = "tabula: this operation requires a registered statement"
