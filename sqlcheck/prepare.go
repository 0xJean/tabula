package sqlcheck

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/internal/pgstmt"
)

// PrepareStatement has the server PREPARE query inside tx, optionally as role,
// then EXPLAIN EXECUTE it with NULL arguments. PREPARE checks names, types,
// functions and parameter inference. EXPLAIN checks plan-time access without
// executing the statement. Runtime function and sequence use needs execution probes.
// Row-level security policies are not evaluated. Everything is undone through
// a savepoint, so tx stays usable for the next statement.
func PrepareStatement(ctx context.Context, tx *sql.Tx, query, role string) (err error) {
	if _, ok := pgstmt.DML(query); !ok {
		return errors.New("expected one preparable data statement")
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT tabula_sqlcheck`); err != nil {
		return err
	}
	prepared := false
	name := "tabula_check_" + strings.ToLower(rand.Text())
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, rollbackErr := tx.ExecContext(cleanup, `ROLLBACK TO SAVEPOINT tabula_sqlcheck`)
		var deallocateErr error
		if prepared {
			_, deallocateErr = tx.ExecContext(cleanup, "DEALLOCATE "+name)
		}
		_, releaseErr := tx.ExecContext(cleanup, `RELEASE SAVEPOINT tabula_sqlcheck`)
		err = errors.Join(err, rollbackErr, deallocateErr, releaseErr)
	}()
	if err := auditRole(ctx, tx, role); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "SET LOCAL plan_cache_mode = force_generic_plan"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "PREPARE "+name+" AS "+query); err != nil {
		return err
	}
	prepared = true
	var params int
	if err := tx.QueryRowContext(ctx,
		`SELECT cardinality(parameter_types) FROM pg_catalog.pg_prepared_statements WHERE name = $1`, name).Scan(&params); err != nil {
		return err
	}
	explain := "EXPLAIN EXECUTE " + name
	if params > 0 {
		explain += `(` + strings.TrimSuffix(strings.Repeat("NULL,", params), ",") + `)`
	}
	rows, err := tx.QueryContext(ctx, explain)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		// Drain EXPLAIN output rows.
	}
	return rows.Err()
}

// VerifyStatements prepares and explains every statement of defs as role
// (empty means the connection's own role) inside one rolled-back transaction
// and returns one line per failing statement, sorted.
func VerifyStatements(ctx context.Context, db *sql.DB, defs []tabula.Definition, role string) ([]string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var failures []string
	for _, d := range defs {
		for key, query := range d.Statements {
			if err := PrepareStatement(ctx, tx, query, role); err != nil {
				failures = append(failures, fmt.Sprintf("%s %s: %v", d.Name, key, err))
			}
		}
	}
	slices.Sort(failures)
	return failures, nil
}

// VerifyViewStatements prepares and explains every registered statement across
// views as role inside one rolled-back transaction.
func VerifyViewStatements(ctx context.Context, db *sql.DB, views []tabula.ViewDefinition, role string) ([]string, error) {
	return VerifyStatements(ctx, db, ViewsToDefinitions(views), role)
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
