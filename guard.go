package tabula

import "github.com/0xJean/tabula/internal/pgstmt"

var checkSessionEscape = compileSessionGuard(SessionGuardConfig{
	ForbidRole:                 true,
	ForbidSessionAuthorization: true,
	ForbidSetConfig:            true,
})

// GuardNoSessionEscape rejects role and session-authorization changes, RESET ALL,
// DISCARD ALL, and calls to set_config. It checks each statement and rejects
// Unicode-escaped identifiers. Ordinary quoted identifiers, literals and comments
// follow PostgreSQL token boundaries.
//
// Pass it directly to stdsql.ScopePolicy.GuardQuery or
// pgxdriver.ScopePolicy.GuardQuery.
func GuardNoSessionEscape(query string) error {
	return checkSessionEscape(pgstmt.Lex(query))
}

// GuardReadOnly admits a single SELECT or WITH statement and applies
// GuardNoSessionEscape. Adapters must also use a read-only PostgreSQL
// connection to enforce write restrictions inside CTEs and functions.
func GuardReadOnly(query string) error {
	tokens, ok := pgstmt.DML(query)
	if !ok || (tokens[0].Text != "select" && tokens[0].Text != "with") {
		return ErrReadOnly
	}
	return checkSessionEscape(tokens)
}
