package sqlcheck

import (
	"context"
	"database/sql"
	"strings"

	"github.com/0xJean/tabula"
)

// PrepareStatement has the server PREPARE query inside tx, optionally as role,
// then EXPLAIN EXECUTE it with NULL arguments. PREPARE checks names, types,
// functions and parameter inference. EXPLAIN checks plan-time access without
// executing the statement. Runtime function and sequence use needs execution probes.
// Row-level security policies are not evaluated. Everything is undone through
// a savepoint, so tx stays usable for the next statement.
func PrepareStatement(ctx context.Context, tx *sql.Tx, query, role string) error {
	return prepareShape(ctx, tx, query, role, nil)
}

func prepareShape(ctx context.Context, tx *sql.Tx, query, role string, shape *tabula.StatementShape) error {
	return prepareUsingShape(ctx, sqlVerificationTx{tx}, query, role, shape)
}

// VerifyStatements prepares and explains every statement of defs as role
// (empty means the connection's own role) inside one rolled-back transaction
// and returns one line per failing statement, sorted. A result-bearing write
// returns ErrDescribeRequired; use VerifyStatementsUsing with a protocol backend.
func VerifyStatements(ctx context.Context, db *sql.DB, defs []tabula.Definition, role string) ([]string, error) {
	return VerifyStatementsUsing(ctx, sqlVerifier{db}, defs, role)
}

// VerifyViewStatements prepares and explains every registered statement across
// views as role inside one rolled-back transaction.
func VerifyViewStatements(ctx context.Context, db *sql.DB, views []tabula.ViewDefinition, role string) ([]string, error) {
	return VerifyStatements(ctx, db, ViewsToDefinitions(views), role)
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
