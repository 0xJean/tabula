package pgstmt

import (
	"strings"
	"unicode"
)

// Tokens extracts lowercase SQL keywords, identifiers, '(', and ';' while
// skipping -- line comments, /* block comments */, and '...' literals.
func Tokens(q string) []string {
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
		if c == '$' {
			end := i + 1
			if end < n && isIdentStart(q[end]) {
				end++
				for end < n && (isIdentStart(q[end]) || q[end] >= '0' && q[end] <= '9') {
					end++
				}
			}
			if end < n && q[end] == '$' {
				delim := q[i : end+1]
				if size := strings.Index(q[end+1:], delim); size >= 0 {
					i = end + 1 + size + len(delim)
					continue
				}
			}
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

// SessionChange identifies a direct attempt to set the role or change session
// configuration recognized by GuardNoSessionEscape.
func SessionChange(tokens []string) string {
	stmtStart := true
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok == ";" {
			stmtStart = true
			continue
		}
		if tok == "set_config" && i+1 < len(tokens) && tokens[i+1] == "(" {
			return "set_config"
		}
		if stmtStart {
			switch tok {
			case "reset":
				if i+1 < len(tokens) && (tokens[i+1] == "role" || tokens[i+1] == "all" || tokens[i+1] == "session") {
					return "RESET " + strings.ToUpper(tokens[i+1])
				}
			case "set":
				next := i + 1
				if next+1 < len(tokens) && tokens[next] == "session" && tokens[next+1] == "authorization" {
					return "SET SESSION AUTHORIZATION"
				}
				if next < len(tokens) && (tokens[next] == "local" || tokens[next] == "session") {
					next++
				}
				if next < len(tokens) && tokens[next] == "role" {
					return "SET ROLE"
				}
				if next+1 < len(tokens) && tokens[next] == "session" && tokens[next+1] == "authorization" {
					return "SET SESSION AUTHORIZATION"
				}
			}
			stmtStart = false
		}
	}
	return ""
}
