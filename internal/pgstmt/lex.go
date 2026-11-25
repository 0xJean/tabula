package pgstmt

import (
	"strings"
	"unicode/utf8"
)

// Token retains the boundaries of identifiers, literals and punctuation.
// Words are folded as PostgreSQL folds unquoted ASCII identifiers.
type Token struct {
	Kind byte
	Text string
}

const (
	Word    byte = 'w'
	Quoted  byte = 'i'
	Literal byte = 's'
	Symbol  byte = 'p'
)

// Lex scans application SQL with standard_conforming_strings enabled.
func Lex(q string) []Token {
	tokens, _ := lex(q, false)
	return tokens
}

// portable rejects quote boundaries that depend on the connection's treatment
// of backslashes in ordinary strings. Explicit E strings specify their own mode.
func lex(q string, portable bool) ([]Token, bool) {
	var out []Token
	complete := true
	for i := 0; i < len(q); {
		start := i
		c := q[i]
		i++
		switch {
		case strings.ContainsRune(" \t\r\n\f\v", rune(c)):
			continue
		case strings.HasPrefix(q[start:], "--"):
			for i < len(q) && q[i] != '\n' && q[i] != '\r' {
				i++
			}
		case strings.HasPrefix(q[start:], "/*"):
			i++
			depth := 1
			for i < len(q) && depth > 0 {
				switch {
				case strings.HasPrefix(q[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(q[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
			complete = complete && depth == 0
		case c == '"' || c == '\'' || ((c == 'e' || c == 'E') && i < len(q) && q[i] == '\''):
			escape := c == 'e' || c == 'E'
			kind := Literal
			if escape {
				c = '\''
				i++
			}
			if c == '"' {
				kind = Quoted
			}
			var b strings.Builder
			closed := false
			for i < len(q) {
				ch := q[i]
				i++
				if escape && ch == '\\' && i < len(q) {
					b.WriteByte(ch)
					b.WriteByte(q[i])
					i++
					continue
				}
				if portable && c == '\'' && !escape && ch == '\\' {
					startSlash := i - 1
					for i < len(q) && q[i] == '\\' {
						i++
					}
					if i < len(q) && q[i] == '\'' && (i-startSlash)%2 != 0 {
						return nil, false
					}
					b.WriteString(q[startSlash:i])
					continue
				}
				if ch == c {
					if i == len(q) || q[i] != c {
						closed = true
						break
					}
					i++
				}
				b.WriteByte(ch)
			}
			complete = complete && closed
			out = append(out, Token{kind, b.String()})
		case c == '$':
			end := i
			if end < len(q) && identStart(q[end]) {
				for end < len(q) && (identStart(q[end]) || digit(q[end])) {
					end++
				}
			}
			if end < len(q) && q[end] == '$' {
				delim := q[start : end+1]
				i = end + 1
				n := strings.Index(q[i:], delim)
				if n < 0 {
					complete = false
					n = len(q) - i
				}
				out = append(out, Token{Literal, q[i : i+n]})
				i = min(len(q), i+n+len(delim))
			} else {
				for i < len(q) && digit(q[i]) {
					i++
				}
				out = append(out, Token{Symbol, q[start:i]})
			}
		case (c == 'u' || c == 'U') && strings.HasPrefix(q[i:], `&"`):
			// A Unicode identifier requires the prefix to touch the opening quote.
			out = append(out, Token{Symbol, "u&"})
			i++
		case identStart(c):
			for i < len(q) && (identStart(q[i]) || digit(q[i]) || q[i] == '$') {
				i++
			}
			name := strings.Map(func(r rune) rune {
				if r >= 'A' && r <= 'Z' {
					return r + ('a' - 'A')
				}
				return r
			}, q[start:i])
			out = append(out, Token{Word, name})
		default:
			out = append(out, Token{Symbol, q[start:i]})
		}
	}
	return out, complete
}

func identStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= utf8.RuneSelf
}
func digit(c byte) bool { return c >= '0' && c <= '9' }

// DML returns the tokens of a single preparable data statement.
func DML(q string) ([]Token, bool) {
	return dataStatement(q, false)
}

// SingleDML checks complete, encoding-independent quote boundaries in UTF-8
// application SQL before a driver can use PostgreSQL's simple query protocol.
func SingleDML(q string) ([]Token, bool) {
	return dataStatement(q, true)
}

func dataStatement(q string, portable bool) ([]Token, bool) {
	if strings.IndexByte(q, 0) >= 0 || !utf8.ValidString(q) {
		return nil, false
	}
	t, ok := lex(q, portable)
	if !ok {
		return nil, false
	}
	if len(t) > 0 && t[len(t)-1] == (Token{Symbol, ";"}) {
		t = t[:len(t)-1]
	}
	head := 0
	for head < len(t) && t[head] == (Token{Symbol, "("}) {
		head++
	}
	if head == len(t) || t[head].Kind != Word {
		return nil, false
	}
	switch t[head].Text {
	case "select", "values", "table", "insert", "update", "delete", "merge", "with":
	default:
		return nil, false
	}
	depth := 0
	for _, tok := range t {
		switch tok {
		case Token{Symbol, ";"}:
			return nil, false
		case Token{Symbol, "("}:
			depth++
		case Token{Symbol, ")"}:
			depth--
			if depth < 0 {
				return nil, false
			}
		}
	}
	if depth != 0 {
		return nil, false
	}
	return t, true
}
