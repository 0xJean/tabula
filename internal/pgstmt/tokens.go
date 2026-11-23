package pgstmt

import "strings"

// Tokens returns the tokens used by the session and read-only guards.
func Tokens(q string) []string {
	var out []string
	for _, t := range Lex(q) {
		if t.Kind == Word || t.Kind == Quoted || (t.Kind == Symbol && (t.Text == ";" || t.Text == "(")) {
			out = append(out, strings.ToLower(t.Text))
		}
	}
	return out
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
