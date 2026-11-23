package tabula

import (
	"fmt"
	"strings"
	"unicode"
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
	tokens := sqlTokens(query)
	stmtStart := true
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok == ";" {
			stmtStart = true
			continue
		}
		if tok == "set_config" && i+1 < len(tokens) && tokens[i+1] == "(" {
			return fmt.Errorf("%w: set_config", ErrForbiddenStatement)
		}
		if stmtStart {
			switch tok {
			case "reset":
				if i+1 < len(tokens) && (tokens[i+1] == "role" || tokens[i+1] == "all" || tokens[i+1] == "session") {
					return fmt.Errorf("%w: RESET %s", ErrForbiddenStatement, strings.ToUpper(tokens[i+1]))
				}
			case "set":
				next := i + 1
				if next+1 < len(tokens) && tokens[next] == "session" && tokens[next+1] == "authorization" {
					return fmt.Errorf("%w: SET SESSION AUTHORIZATION", ErrForbiddenStatement)
				}
				if next < len(tokens) && (tokens[next] == "local" || tokens[next] == "session") {
					next++
				}
				if next < len(tokens) && tokens[next] == "role" {
					return fmt.Errorf("%w: SET ROLE", ErrForbiddenStatement)
				}
				if next+1 < len(tokens) && tokens[next] == "session" && tokens[next+1] == "authorization" {
					return fmt.Errorf("%w: SET SESSION AUTHORIZATION", ErrForbiddenStatement)
				}
			}
			stmtStart = false
		}
	}
	return nil
}

// sqlTokens extracts lowercase SQL keywords, identifiers, '(', and ';' while
// skipping -- line comments, /* block comments */, and '...' literals.
func sqlTokens(q string) []string {
	var out []string
	n := len(q)
	for i := 0; i < n; {
		c := q[i]
		if c == '-' && i+1 < n && q[i+1] == '-' {
			i += 2
			for i < n && q[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < n && q[i+1] == '*' {
			i += 2
			depth := 1
			for i < n && depth > 0 {
				if q[i] == '/' && i+1 < n && q[i+1] == '*' {
					depth++
					i += 2
					continue
				}
				if q[i] == '*' && i+1 < n && q[i+1] == '/' {
					depth--
					i += 2
					continue
				}
				i++
			}
			continue
		}
		if c == '\'' {
			i++
			for i < n {
				if q[i] == '\'' {
					i++
					if i < n && q[i] == '\'' {
						i++
						continue
					}
					break
				}
				i++
			}
			continue
		}
		if c == '"' {
			i++
			var b strings.Builder
			for i < n {
				if q[i] == '"' {
					i++
					if i < n && q[i] == '"' {
						b.WriteByte('"')
						i++
						continue
					}
					break
				}
				b.WriteByte(byte(unicode.ToLower(rune(q[i]))))
				i++
			}
			if b.Len() > 0 {
				out = append(out, b.String())
			}
			continue
		}
		if c == '(' || c == ';' {
			out = append(out, string(c))
			i++
			continue
		}
		if isIdentStart(c) {
			start := i
			i++
			for i < n && isIdentPart(q[i]) {
				i++
			}
			out = append(out, strings.ToLower(q[start:i]))
			continue
		}
		i++
	}
	return out
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '$'
}
