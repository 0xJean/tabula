package sqlcheck

import (
	"context"
	"errors"
)

// ErrDescribeRequired reports that a statement's result cannot be checked
// without a non-executing protocol description. In particular, LIMIT 0 is not
// a safe substitute for describing a statement containing a modifying CTE.
var ErrDescribeRequired = errors.New("sqlcheck: non-executing result description is required")

// Verifier begins an isolated read-only verification transaction. Implementations
// keep catalog reads, role setup, statement description and EXPLAIN on the same
// connection. This capability grants no runtime statement authority.
type Verifier interface {
	BeginVerification(context.Context) (VerificationTx, error)
}

// VerificationTx is the standard-library-only connection contract for statement
// verification. Describe must parse and describe without executing the supplied
// statement. Rollback releases the transaction and all connection resources.
type VerificationTx interface {
	Exec(ctx context.Context, sql string, args ...any) error
	Scan(ctx context.Context, sql string, args []any, dest ...any) error
	Each(ctx context.Context, sql string, args []any, visit func(scan func(...any) error) error) error
	Describe(ctx context.Context, statementName, sql string) (Description, error)
	Rollback(context.Context) error
}

// Description contains a prepared statement's parameter OIDs and ordered wire
// result fields. Server-local OIDs are evidence for this verification only and
// must not be persisted as portable schema identities.
type Description struct {
	Parameters []uint32
	Fields     []DescribedField
}

// DescribedField is a PostgreSQL RowDescription field. TableOID and Attribute
// may be zero for computed values. PostgreSQL may report a domain's base type;
// TypeOID alone does not establish its logical domain identity or nullability.
type DescribedField struct {
	Name      string
	TypeOID   uint32
	TypeMod   int32
	TableOID  uint32
	Attribute int16
}
