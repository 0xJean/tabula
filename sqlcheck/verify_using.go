package sqlcheck

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/0xJean/tabula"
)

// VerifyRoleStatementsUsing prepares, describes and explains each role's SQL
// through a non-executing verification backend. Result-bearing writes require
// Describe; they are never wrapped in a SELECT probe. Findings are sorted.
func VerifyRoleStatementsUsing(ctx context.Context, verifier Verifier, work RoleStatements) (findings []string, err error) {
	if err := work.validate(); err != nil {
		return nil, err
	}
	return verifyUsing(ctx, verifier, func(tx VerificationTx) ([]string, error) {
		var findings []string
		for _, role := range slices.Sorted(maps.Keys(work)) {
			for _, s := range work[role] {
				if err := prepareUsingShape(ctx, tx, s.SQL, role, s.Shape); err != nil {
					detail := fmt.Errorf("%s/%s: %w", role, s.Name, err)
					if verificationFatal(err) {
						return findings, detail
					}
					findings = append(findings, detail.Error())
				}
			}
		}
		slices.Sort(findings)
		return findings, ctx.Err()
	})
}

// VerifyStatementsUsing verifies every registered statement through verifier.
// An empty role retains the connection's own role. A database/sql verifier can
// check legacy reads, but typed write results require a Describe backend.
func VerifyStatementsUsing(ctx context.Context, verifier Verifier, defs []tabula.Definition, role string) ([]string, error) {
	return verifyUsing(ctx, verifier, func(tx VerificationTx) ([]string, error) {
		var failures []string
		for _, d := range defs {
			for _, key := range slices.Sorted(maps.Keys(d.Statements)) {
				var shape *tabula.StatementShape
				if s, ok := d.StatementShapes[key]; ok {
					shape = &s
				}
				if err := prepareUsingShape(ctx, tx, d.Statements[key], role, shape); err != nil {
					detail := fmt.Errorf("%s %s: %w", d.Name, key, err)
					if verificationFatal(err) {
						return failures, detail
					}
					failures = append(failures, detail.Error())
				}
			}
		}
		slices.Sort(failures)
		return failures, ctx.Err()
	})
}

func verifyUsing(ctx context.Context, verifier Verifier, check func(VerificationTx) ([]string, error)) (findings []string, err error) {
	if verifier == nil {
		return nil, errors.New("sqlcheck: verifier is required")
	}
	tx, err := verifier.BeginVerification(ctx)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, errors.New("sqlcheck: verifier returned no transaction")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		err = errors.Join(err, tx.Rollback(cleanup))
	}()
	return check(tx)
}

type verificationCleanupError struct{ err error }

func (e *verificationCleanupError) Error() string {
	return "sqlcheck: verification cleanup: " + e.err.Error()
}
func (e *verificationCleanupError) Unwrap() error { return e.err }
func verificationFatal(err error) bool {
	_, cleanup := errors.AsType[*verificationCleanupError](err)
	return cleanup || errors.Is(err, ErrDescribeRequired)
}
