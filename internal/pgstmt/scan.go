package pgstmt

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// Param marks a PostgreSQL positional parameter placeholder such as $1.
	Param byte = '$'
	// Comment marks a PostgreSQL line (--...) or block (/*...*/) comment.
	Comment byte = 'c'
)

// Span is a source-indexed lexical token or comment from ScanFragment.
type Span struct {
	Kind     byte
	Text     string
	Start    int
	End      int
	ParamNum int
}

// ScanFragment tokenizes a SQL fragment while preserving exact source offsets,
// comments and string/identifier/dollar-quote boundaries. It rejects
// unterminated strings, identifiers, dollar-quotes or block comments,
// statement separators, unbalanced parentheses, invalid $0/overflow parameter
// slots, and trailing line comments without a terminating newline.
func ScanFragment(q string) ([]Span, error) {
	var out []Span
	parenDepth := 0
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
			if i == len(q) {
				return nil, errors.New("trailing line comment without newline")
			}
			out = append(out, Span{Kind: Comment, Text: q[start:i], Start: start, End: i})
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
			if depth != 0 {
				return nil, errors.New("unterminated block comment")
			}
			out = append(out, Span{Kind: Comment, Text: q[start:i], Start: start, End: i})
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
			closed := false
			for i < len(q) {
				ch := q[i]
				i++
				if escape && ch == '\\' {
					if i >= len(q) {
						return nil, errors.New("unterminated escape string literal")
					}
					i++
					continue
				}
				if ch == c {
					if i < len(q) && q[i] == c {
						i++
						continue
					}
					closed = true
					break
				}
			}
			if !closed {
				if kind == Quoted {
					return nil, errors.New("unterminated quoted identifier")
				}
				return nil, errors.New("unterminated string literal")
			}
			out = append(out, Span{Kind: kind, Text: q[start:i], Start: start, End: i})
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
					return nil, errors.New("unterminated dollar-quoted string")
				}
				i += n + len(delim)
				out = append(out, Span{Kind: Literal, Text: q[start:i], Start: start, End: i})
				continue
			}
			if i < len(q) && digit(q[i]) {
				for i < len(q) && digit(q[i]) {
					i++
				}
				num, err := strconv.Atoi(q[start+1 : i])
				if err != nil || num <= 0 || num > 65535 {
					return nil, fmt.Errorf("invalid parameter placeholder %s", q[start:i])
				}
				out = append(out, Span{Kind: Param, Text: q[start:i], Start: start, End: i, ParamNum: num})
				continue
			}
			out = append(out, Span{Kind: Symbol, Text: "$", Start: start, End: i})
		case (c == 'u' || c == 'U') && strings.HasPrefix(q[i:], `&"`):
			out = append(out, Span{Kind: Symbol, Text: q[start : i+1], Start: start, End: i + 1})
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
			out = append(out, Span{Kind: Word, Text: name, Start: start, End: i})
		default:
			switch c {
			case ';':
				return nil, errors.New("statement separator ';' is not allowed in SQL fragment")
			case '(':
				parenDepth++
			case ')':
				parenDepth--
				if parenDepth < 0 {
					return nil, errors.New("unbalanced closing parenthesis")
				}
			}
			out = append(out, Span{Kind: Symbol, Text: q[start:i], Start: start, End: i})
		}
	}
	if parenDepth != 0 {
		return nil, errors.New("unbalanced opening parenthesis")
	}
	return out, nil
}

// RewriteParams replaces each Param span in q using remap while preserving all
// other source bytes verbatim.
func RewriteParams(q string, spans []Span, remap func(slot int) (int, error)) (string, error) {
	var b strings.Builder
	b.Grow(len(q) + 16)
	cursor := 0
	for _, sp := range spans {
		if sp.Kind != Param {
			continue
		}
		target, err := remap(sp.ParamNum)
		if err != nil {
			return "", err
		}
		b.WriteString(q[cursor:sp.Start])
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(target))
		cursor = sp.End
	}
	b.WriteString(q[cursor:])
	return b.String(), nil
}
