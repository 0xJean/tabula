package sqlcheck

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/internal/pgstmt"
)

func prepareUsingShape(ctx context.Context, tx VerificationTx, query, role string, shape *tabula.StatementShape) (err error) {
	if _, ok := pgstmt.DML(query); !ok {
		return errors.New("expected one preparable data statement")
	}
	name := "tabula_check_" + strings.ToLower(rand.Text())
	savepoint := name + "_sp"
	if err := tx.Exec(ctx, "SAVEPOINT "+savepoint); err != nil {
		return err
	}
	prepared := false
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		rollbackErr := tx.Exec(cleanup, "ROLLBACK TO SAVEPOINT "+savepoint)
		var deallocateErr error
		if prepared {
			deallocateErr = tx.Exec(cleanup, "DEALLOCATE "+name)
		}
		releaseErr := tx.Exec(cleanup, "RELEASE SAVEPOINT "+savepoint)
		if failure := errors.Join(rollbackErr, deallocateErr, releaseErr); failure != nil {
			err = errors.Join(err, &verificationCleanupError{failure})
		}
	}()
	if err := auditVerificationRole(ctx, tx, role); err != nil {
		return err
	}
	if err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan"); err != nil {
		return err
	}
	if err := tx.Exec(ctx, "PREPARE "+name+" AS "+query); err != nil {
		return err
	}
	prepared = true
	var params int
	if err := tx.Scan(ctx, `SELECT cardinality(parameter_types) FROM pg_catalog.pg_prepared_statements WHERE name = $1`, []any{name}, &params); err != nil {
		return err
	}
	if shape != nil {
		if err := checkVerificationShape(ctx, tx, name, query, params, shape); err != nil {
			return err
		}
	}
	explain := "EXPLAIN (VERBOSE, FORMAT JSON) EXECUTE " + name
	if params > 0 {
		explain += "(" + strings.TrimSuffix(strings.Repeat("NULL,", params), ",") + ")"
	}
	var plan string
	if err := tx.Scan(ctx, explain, nil, &plan); err != nil {
		return err
	}
	if shape != nil {
		return checkWritePlan(ctx, tx, shape, json.RawMessage(plan))
	}
	return nil
}

func auditVerificationRole(ctx context.Context, tx VerificationTx, role string) error {
	if role != "" {
		if err := tx.Exec(ctx, "SET LOCAL ROLE "+quoteIdent(role)); err != nil {
			return err
		}
	}
	var effective, setting string
	if err := tx.Scan(ctx, "SELECT current_user, pg_catalog.set_config('standard_conforming_strings','on',true)", nil, &effective, &setting); err != nil {
		return err
	}
	if role != "" && effective != role {
		return fmt.Errorf("requested role %q, PostgreSQL selected %q", role, effective)
	}
	return nil
}

// Metadata cannot make visible DML safe to execute as a read probe.
func descriptionNeedsProtocol(query string, s *tabula.StatementShape) bool {
	if s.Effects&tabula.EffectWrite != 0 {
		return true
	}
	for _, routine := range s.Routines {
		if !routine.ReadOnly {
			return true
		}
	}
	guard, err := (tabula.StatementPolicy{Read: []string{"select", "table", "values", "with"}}).Compile(true)
	return err != nil || guard.Check("Scan", query) != nil
}
