package tabula

import "context"

// OperationSession provides a savepoint-scoped operation within an existing
// transaction. The callback receives a sealed child session and its context.
// An error or panic rolls back the operation; cleanup failures must prevent the
// enclosing transaction from committing. Cancellation first observed after a
// successful callback is a completion failure and must also prevent commit.
// The parent cannot execute concurrently
// with the child, and the child is closed when the operation ends.
// A callback error matching ErrUnobservedWrite must also prevent the enclosing
// transaction from committing, after the operation has been rolled back.
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

// ErrUnobservedWrite marks a write failure discovered while draining results
// after an iterator's consumer has stopped. The operation is rolled back and
// the enclosing transaction must fail, because the iterator cannot yield again.
// The wrapped database error retains its SQLSTATE and constraint diagnostics.
const ErrUnobservedWrite staticError = "tabula: write failed after result consumption stopped"
