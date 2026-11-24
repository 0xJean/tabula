package tabula

import (
	"context"
	"errors"
	"reflect"
	"strings"
)

type staticError string

func (e staticError) Error() string { return string(e) }

type readOnlyPolicyError string

func (e readOnlyPolicyError) Error() string { return string(e) }
func (readOnlyPolicyError) Unwrap() error   { return ErrReadOnly }

const (
	// Write-guard and optimistic concurrency sentinel errors (used when
	// ErrorPolicy leaves the corresponding slot nil).
	ErrConflict           staticError = "row conflict or lost update"
	ErrOnce               staticError = "insert-only column changed on update"
	ErrNotDeleted         staticError = "soft delete needs the deleted column set"
	ErrRowCount           staticError = "write did not confirm exactly one row"
	ErrReadOnly           staticError = "operation requires a writable session and table"
	ErrScopeMismatch      staticError = "row scope differs from the bound session"
	ErrForbiddenStatement staticError = "statement attempts to modify session role or configuration"
	ErrForeignSession     staticError = "session was not issued for this registry"
	ErrNested             staticError = "nested transaction is not allowed"
	ErrSessionClosed      staticError = "session is closed"
	ErrCapabilityDenied   staticError = "operation denied by registry capability policy"

	// ErrUnscopedRequiresReadOnly reports that UnscopedReadOnly denied access
	// because the transaction is read-write. It also matches ErrReadOnly.
	ErrUnscopedRequiresReadOnly readOnlyPolicyError = "UnscopedReadOnly requires a read-only transaction"

	// ErrSessionBusy reports an overlapping operation on a transaction.
	// Finish the active statement or row iteration before using the transaction again.
	ErrSessionBusy staticError = "another operation is using the transaction"

	// PostgreSQL error-class sentinels returned by default error classification.
	ErrUnique                staticError = "unique constraint violation"
	ErrForeignKey            staticError = "foreign key constraint violation"
	ErrCheck                 staticError = "check constraint violation"
	ErrNotNull               staticError = "not-null constraint violation"
	ErrSerialization         staticError = "serialization failure"
	ErrDeadlock              staticError = "deadlock detected"
	ErrRLSDenied             staticError = "row-level security policy denied the operation"
	ErrInsufficientPrivilege staticError = "insufficient PostgreSQL privilege"
	ErrCanceled              staticError = "statement canceled"
)

// ErrorClass is the fixed error classification string used in structured logs
// and error inspection.
type ErrorClass string

const (
	ClassUnique                ErrorClass = "unique"
	ClassForeignKey            ErrorClass = "foreign_key"
	ClassCheck                 ErrorClass = "check"
	ClassNotNull               ErrorClass = "not_null"
	ClassSerialization         ErrorClass = "serialization"
	ClassDeadlock              ErrorClass = "deadlock"
	ClassCanceled              ErrorClass = "canceled"
	ClassRLSDenied             ErrorClass = "rls_denied"
	ClassInsufficientPrivilege ErrorClass = "insufficient_privilege"
	ClassOther                 ErrorClass = "other"
)

// SQLState returns the five-character PostgreSQL SQLSTATE carried by err
// (compatible with both lib/pq and pgx errors implementing SQLState() string),
// or "" when err carries none.
func SQLState(err error) string {
	if pg, ok := errors.AsType[interface {
		error
		SQLState() string
	}](err); ok {
		return pg.SQLState()
	}
	return ""
}

// ClassifyError inspects ctx and err (by context cancellation or PostgreSQL
// SQLSTATE) and returns its ErrorClass plus the matching tabula sentinel error
// (nil for ClassOther).
func ClassifyError(ctx context.Context, err error) (ErrorClass, error) {
	if err == nil {
		return "", nil
	}
	if (ctx != nil && ctx.Err() != nil) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		SQLState(err) == "57014" {
		return ClassCanceled, ErrCanceled
	}
	switch SQLState(err) {
	case "42501":
		if rlsViolation(err) {
			return ClassRLSDenied, ErrRLSDenied
		}
		return ClassInsufficientPrivilege, ErrInsufficientPrivilege
	case "23505":
		return ClassUnique, ErrUnique
	case "23503":
		return ClassForeignKey, ErrForeignKey
	case "23514":
		return ClassCheck, ErrCheck
	case "23502":
		return ClassNotNull, ErrNotNull
	case "40001":
		return ClassSerialization, ErrSerialization
	case "40P01":
		return ClassDeadlock, ErrDeadlock
	default:
		return ClassOther, nil
	}
}

// IsUniqueViolation reports PostgreSQL unique_violation (SQLSTATE 23505).
func IsUniqueViolation(err error) bool { return SQLState(err) == "23505" }

// IsForeignKeyViolation reports PostgreSQL foreign_key_violation (SQLSTATE 23503).
func IsForeignKeyViolation(err error) bool { return SQLState(err) == "23503" }

// IsCheckViolation reports PostgreSQL check_violation (SQLSTATE 23514).
func IsCheckViolation(err error) bool { return SQLState(err) == "23514" }

// IsNotNullViolation reports PostgreSQL not_null_violation (SQLSTATE 23502).
func IsNotNullViolation(err error) bool { return SQLState(err) == "23502" }

// IsSerializationFailure reports PostgreSQL serialization_failure (SQLSTATE 40001).
func IsSerializationFailure(err error) bool { return SQLState(err) == "40001" }

// IsDeadlock reports PostgreSQL deadlock_detected (SQLSTATE 40P01).
func IsDeadlock(err error) bool { return SQLState(err) == "40P01" }

// IsQueryCanceled reports PostgreSQL query_canceled (SQLSTATE 57014).
func IsQueryCanceled(err error) bool { return SQLState(err) == "57014" }

// IsRejectedValue reports a value refused by column definitions: string data
// right truncation (22001), numeric value out of range (22003), not_null
// violation (23502), check violation (23514), or invalid text representation
// (22P02).
func IsRejectedValue(err error) bool {
	switch SQLState(err) {
	case "22001", "22003", "23502", "23514", "22P02":
		return true
	}
	return false
}

// IsNumericOutOfRange reports SQLSTATE 22003.
func IsNumericOutOfRange(err error) bool { return SQLState(err) == "22003" }

// rlsViolation uses the PostgreSQL source routine when a driver exposes it.
// lib/pq and pgx both carry the Routine field. Other drivers can supply the
// standard English primary diagnostic. An ambiguous 42501 stays generic.
func rlsViolation(err error) bool {
	pg, ok := errors.AsType[interface {
		error
		SQLState() string
	}](err)
	if !ok {
		return false
	}
	v := reflect.ValueOf(pg)
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() == reflect.Struct {
		r := v.FieldByName("Routine")
		if r.IsValid() && r.Kind() == reflect.String && r.String() != "" {
			return r.String() == "ExecWithCheckOptions" || r.String() == "check_enable_rls"
		}
	}
	msg := pg.Error()
	for _, prefix := range []string{"pq: ", "ERROR: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	return strings.HasPrefix(msg, "new row violates row-level security policy") || strings.HasPrefix(msg, "query would be affected by row-level security policy")
}
