package tabula

import (
	"fmt"
	"slices"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// GuardNoSessionEscape rejects SQL queries that attempt to switch PostgreSQL
// roles or mutate session configuration from application/adapter SQL:
// SET [LOCAL|SESSION] ROLE, RESET ROLE, RESET ALL, SESSION AUTHORIZATION, and
// set_config(...). Statement boundaries (;) are tracked so column assignments
// like "UPDATE users SET role = $2" are allowed while standalone or stacked
// "SET ROLE ..." / "SET role = ..." statements are rejected. Comments and
// single-quoted string literals are ignored to avoid false positives.
//
// Pass it directly to stdsql.ScopePolicy.GuardQuery or
// pgxdriver.ScopePolicy.GuardQuery.
func GuardNoSessionEscape(query string) error {
	return guardSessionChange(pgstmt.Tokens(query))
}

// GuardReadOnly admits a single SELECT or WITH statement and applies
// GuardNoSessionEscape. Adapters must also use a read-only PostgreSQL
// connection to enforce write restrictions inside CTEs and functions.
func GuardReadOnly(query string) error {
	tokens := pgstmt.Tokens(query)
	if len(tokens) == 0 || (tokens[0] != "select" && tokens[0] != "with") {
		return ErrReadOnly
	}
	if slices.Contains(tokens[:len(tokens)-1], ";") {
		return ErrReadOnly
	}
	return guardSessionChange(tokens)
}

func guardSessionChange(tokens []string) error {
	if op := pgstmt.SessionChange(tokens); op != "" {
		return fmt.Errorf("%w: %s", ErrForbiddenStatement, op)
	}
	return nil
}
