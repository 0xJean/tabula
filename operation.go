package tabula

import "context"

// OperationSession provides a savepoint-scoped operation within an existing
// transaction. The callback receives a sealed child session and its context.
// An error or panic rolls back the operation; cleanup failures must prevent the
// enclosing transaction from committing. The parent cannot execute concurrently
// with the child, and the child is closed when the operation ends.
//
// Plans requiring atomic batches or rollback on a result-decoding failure test
// for this optional capability before I/O. Ordinary Session and AuthorizedSession
// implementations keep their existing method sets.
type OperationSession[S any] interface {
	WithinOperation(context.Context, func(context.Context, Session[S]) error) error
}

// ErrOperationRequired reports that a plan requires an operation savepoint but
// its session does not provide OperationSession. No raw SQL fallback is used.
const ErrOperationRequired staticError = "tabula: session requires atomic operation support"
