# M3 backend and effect contracts

These signatures and safety boundaries are approved for implementation. The shared freeze records the package-private compiler seams alongside them. A contract is not evidence that its implementation or driver acceptance is complete.

## Verification backend

The implementation adapter stays in a driver module. Root sqlcheck imports only standard-library and public core types. The interface keeps role, catalog reads, descriptions and EXPLAIN on one connection and one verification transaction.

```go
// Package sqlcheck.
type Verifier interface {
    BeginVerification(context.Context) (VerificationTx, error)
}

type VerificationTx interface {
    Exec(ctx context.Context, sql string, args ...any) error
    Scan(ctx context.Context, sql string, args []any, dest ...any) error
    Each(ctx context.Context, sql string, args []any,
        visit func(scan func(...any) error) error) error
    Describe(ctx context.Context, statementName, sql string) (Description, error)
    Rollback(context.Context) error
}

type Description struct {
    Parameters []uint32
    Fields     []DescribedField
}

type DescribedField struct {
    Name       string
    TypeOID    uint32
    TypeMod    int32
    TableOID   uint32
    Attribute  int16
}

func VerifyRoleStatementsUsing(ctx context.Context, db Verifier,
    work RoleStatements) ([]string, error)
func VerifyStatementsUsing(ctx context.Context, db Verifier,
    defs []tabula.Definition, role string) ([]string, error)
```

The driver supplies `pgxdriver.SQLVerifier(pool)` as an explicit adapter. It begins a read-only verification transaction. The root algorithm establishes/reset-checks the requested role and settings, opens a per-statement savepoint, prepares/describes the original statement, checks catalogs/metadata and explains without ANALYZE. Describe must not Execute the described statement. On an error, recover to the savepoint, close prepared statements and restore state before continuing. The outer transaction is always rolled back. The existing database/sql verifier can share the algorithm through an internal adapter, while returning ErrDescribeRequired for a result it cannot inspect safely.

The backend is a trusted connection adapter, not runtime authority. Its SQL strings are supplied by the verification algorithm, not a new query execution escape in a Registry. All snapshots still undergo existing lexical and ownership validation. An application-provided backend cannot sign a plan or authorize a Session call.

Compare parameter OIDs independently of result OIDs. A domain parameter and a returned domain column can have different wire representations. `TableOID`/`Attribute` are optional origin evidence, not an automatic permission grant or a fabricated logical type. A field with no origin requires supported closed expression lineage to establish a logical domain; otherwise verification returns an explicit finding. No server-local OID is persisted in a portable snapshot.

## Atomic write-operation hook

The existing generic Session and AuthorizedSession method sets remain unchanged. New operations that promise rollback on partial batch or client decode failure require an additive optional interface, implemented by both shipped drivers:

```go
type OperationSession[S any] interface {
    WithinOperation(ctx context.Context,
        fn func(context.Context, Session[S]) error) error
}
```

This is a savepoint-scoped operation on the existing transaction, not a new transaction or an ambient callback. The callback gets a sealed child session and derived callback context. Calls are bound to that child, and a captured child becomes closed when the operation ends. Scope, issuer, read-only state, managed settings and per-statement role behavior are inherited. The parent is not usable concurrently with the operation; existing busy checks continue to apply. A callback error, decoder error or panic rolls back to the savepoint before returning/republishing the panic. Release/rollback errors retain diagnostic chains. Cleanup cannot leave the runner able to commit an unknown partial operation: a failed cleanup marks it unusable and forces rollback.

The root plan validates original bindings and method eligibility before opening the operation, then signs each call for the child. This avoids accepting already-adapted arguments or reusing parent authorization on another session. An external driver lacking OperationSession fails before I/O for an operation that requires it; there is no raw SQL SAVEPOINT fallback. The two drivers' existing public Savepoint signatures can stay unchanged and delegate internally to the shared lifecycle implementation.

Returning All uses the operation hook when a client-side result failure could otherwise leave a successful write committed. Iter keeps the operation open for the iterator lifetime. Successful early stop means consumption stopped; the write is completed/drained and the savepoint is released. Driver/scan/cancellation failure rolls it back. A leaked pull iterator is closed by the enclosing session lifecycle, with the existing no-deadlock contract. Do not use a read LIMIT to reduce a mutation's returned cardinality.

## Effect-aware policy

Add the two opt-in boolean fields to StatementPolicy:

```go
AllowAuthorizedWriteCTE     bool
AllowAuthorizedWriteRoutine bool
```

The ordinary public Check(method, sql) ignores these permissions because SQL text is not a signature. A separate internal authority-checked path receives the immutable compiled effect record after Open has verified issuer, session and method. The new fields do not themselves turn off SingleStatement, escape guards, ReadOnly, Direct, or configured write-command restrictions. A CTE's entire write-command set must be allowed; a routine's exact signed write requirement must be verified. RequireReturning's outer-syntax rule is bypassed only for the opted-in signed result form. Default false preserves v0.4.3 behavior. Wrong-method, wrong-session, copied text and edited AuthorizedSQL tests are mandatory.

Adding these fields is compatible with keyed struct literals. As with prior additive public config fields, positional StatementPolicy literals need field names. Document this explicitly in MIGRATION and the API diff before release; do not claim full source compatibility for positional literals.

## Snapshot and source-inventory seams

Existing input/result/effect fields remain the core. Freeze additive metadata only after the new relational IR can produce it together with SQL. The metadata must identify result lineage, conflict arbiter premises and any operation requiring a description capability. Every slice/map is copied on export. Optional fields use omitempty where compatible. The new Verify entry points and constructor bindings are recognized by CollectSQL; a query fragment never counts as an execution point by itself.

Unknown required metadata is a verifier failure. The published SQL alone remains inspectable, but an older checker must not be advertised as checking a new domain, conflict, nested key or result contract. Do not add credentials to JSON, error strings, metrics, snapshots or generated SQL comments.
