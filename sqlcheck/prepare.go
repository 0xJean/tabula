package sqlcheck

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/0xJean/tabula"
)

// PrepareStatement has the server PREPARE query inside tx, optionally as role,
// then EXPLAIN EXECUTE it with NULL arguments. PREPARE checks names, types,
// functions and parameter inference; EXPLAIN reaches executor start, where
// table, column and function privileges are checked, without executing writes.
// Row-level security policies are not evaluated. Everything is undone through
// a savepoint, so tx stays usable for the next statement.
func PrepareStatement(ctx context.Context, tx *sql.Tx, query, role string) (err error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT tabula_sqlcheck`); err != nil {
		return err
	}
	defer func() {
		if _, rollbackErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT tabula_sqlcheck`); rollbackErr != nil && err == nil {
			err = rollbackErr
		}
		// PREPARE outlives the savepoint; release it explicitly.
		_, _ = tx.ExecContext(ctx, `DEALLOCATE ALL`)
	}()
	if role != "" {
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+quoteIdent(role)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `PREPARE tabula_sqlcheck_statement AS `+query); err != nil {
		return err
	}
	var params int
	if err := tx.QueryRowContext(ctx,
		`SELECT cardinality(parameter_types) FROM pg_prepared_statements WHERE name = 'tabula_sqlcheck_statement'`).Scan(&params); err != nil {
		return err
	}
	explain := `EXPLAIN EXECUTE tabula_sqlcheck_statement`
	if params > 0 {
		explain += `(` + strings.TrimSuffix(strings.Repeat("NULL,", params), ",") + `)`
	}
	rows, err := tx.QueryContext(ctx, explain)
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	return rows.Close()
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
