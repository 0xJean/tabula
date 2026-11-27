package pgstmt

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// NamedParam marks a named parameter placeholder such as @id.
const NamedParam byte = '@'

// ScanNamed tokenizes PostgreSQL SQL containing @ident named parameters while
// preserving exact source offsets, comments, and string/identifier/dollar-quote
// boundaries. It rejects raw $1 positional placeholders, NUL bytes, invalid
// UTF-8, unterminated strings/identifiers/dollar-quotes/block-comments,
// backslash-quote ambiguity in ordinary strings, unbalanced parentheses, and
// multi-statement separators. When fragment is true, semicolons and trailing
// line comments without a terminating newline are also rejected.
func ScanNamed(q string, fragment bool) ([]Span, error) {
	if strings.IndexByte(q, 0) >= 0 || !utf8.ValidString(q) {
		return nil, errors.New("SQL must be valid UTF-8 without NUL bytes")
	}
	var out []Span
	parenDepth := 0
	seenSemi := false
	hasCode := false
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
			if fragment && i == len(q) {
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
			if seenSemi {
				return nil, errors.New("multiple SQL statements are not allowed")
			}
			hasCode = true
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
				if c == '\'' && !escape && ch == '\\' {
					startSlash := i - 1
					for i < len(q) && q[i] == '\\' {
						i++
					}
					if i < len(q) && q[i] == '\'' && (i-startSlash)%2 != 0 {
						return nil, errors.New("ambiguous backslash before quote in string literal; use E'...' or a parameter")
					}
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
			if seenSemi {
				return nil, errors.New("multiple SQL statements are not allowed")
			}
			hasCode = true
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
				return nil, fmt.Errorf("positional parameter %s is not allowed; use @name", q[start:i])
			}
			out = append(out, Span{Kind: Symbol, Text: "$", Start: start, End: i})
		case c == '@':
			if seenSemi {
				return nil, errors.New("multiple SQL statements are not allowed")
			}
			hasCode = true
			if isNamedParamBefore(q, start) && i < len(q) && asciiIdentStart(q[i]) {
				end := i + 1
				for end < len(q) && asciiIdentPart(q[end]) {
					end++
				}
				if isNamedParamAfter(q, end) {
					out = append(out, Span{Kind: NamedParam, Text: q[i:end], Start: start, End: end})
					i = end
					continue
				}
			}
			out = append(out, Span{Kind: Symbol, Text: "@", Start: start, End: i})
		case (c == 'u' || c == 'U') && strings.HasPrefix(q[i:], `&"`):
			if seenSemi {
				return nil, errors.New("multiple SQL statements are not allowed")
			}
			hasCode = true
			out = append(out, Span{Kind: Symbol, Text: q[start : i+1], Start: start, End: i + 1})
			i++
		case identStart(c):
			if seenSemi {
				return nil, errors.New("multiple SQL statements are not allowed")
			}
			hasCode = true
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
			if c == ';' {
				if fragment || seenSemi || !hasCode || parenDepth != 0 {
					return nil, errors.New("statement separator ';' is not allowed")
				}
				seenSemi = true
				out = append(out, Span{Kind: Symbol, Text: ";", Start: start, End: i})
				continue
			}
			if seenSemi {
				return nil, errors.New("multiple SQL statements are not allowed")
			}
			hasCode = true
			switch c {
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
	if !hasCode {
		return nil, errors.New("SQL statement is empty")
	}
	return out, nil
}

// isNamedParamBefore reports whether the byte immediately preceding '@' is a
// valid named-parameter boundary. It excludes '<' (preserving '<@'), '@'
// (preserving '@@'), '.', '::', quotes, closing delimiters, and identifier or
// digit characters (preserving non-boundary '@' such as email@domain).
func isNamedParamBefore(q string, pos int) bool {
	if pos == 0 {
		return true
	}
	prev := q[pos-1]
	switch prev {
	case ' ', '\t', '\r', '\n', '\f', '\v',
		'(', '[', ',', '=', '>', '+', '-', '*', '/', '%', '!', '~', '|', '&', '^', '?':
		return true
	case ':':
		return pos < 2 || q[pos-2] != ':'
	default:
		return false
	}
}

func isNamedParamAfter(q string, end int) bool {
	if end >= len(q) {
		return true
	}
	next := q[end]
	return next != '@' && next != '$' && next < utf8.RuneSelf
}

func asciiIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func asciiIdentPart(c byte) bool {
	return asciiIdentStart(c) || digit(c)
}

// ValidNamedParam reports whether name is a valid ASCII named parameter identifier.
func ValidNamedParam(name string) bool {
	if name == "" || !asciiIdentStart(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !asciiIdentPart(name[i]) {
			return false
		}
	}
	return true
}

// RewriteNamed replaces each NamedParam span in q with $N returned by slotOf,
// strips an optional trailing top-level semicolon, and preserves all other
// source bytes verbatim.
func RewriteNamed(q string, spans []Span, slotOf func(name string) (int, error)) (string, error) {
	var b strings.Builder
	b.Grow(len(q) + 16)
	cursor := 0
	for _, sp := range spans {
		switch {
		case sp.Kind == NamedParam:
			slot, err := slotOf(sp.Text)
			if err != nil {
				return "", err
			}
			if slot <= 0 || slot > 65535 {
				return "", fmt.Errorf("invalid parameter slot $%d for @%s", slot, sp.Text)
			}
			b.WriteString(q[cursor:sp.Start])
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(slot))
			cursor = sp.End
		case sp.Kind == Symbol && sp.Text == ";":
			b.WriteString(q[cursor:sp.Start])
			cursor = sp.End
		}
	}
	b.WriteString(q[cursor:])
	return b.String(), nil
}

// RewriteNamedSQL scans q and rewrites @name placeholders into dense $1..$n
// slots in first-occurrence order, reusing the same slot for repeated names.
func RewriteNamedSQL(q string, fragment bool) (string, []string, error) {
	spans, err := ScanNamed(q, fragment)
	if err != nil {
		return "", nil, err
	}
	slots := make(map[string]int)
	var order []string
	rewritten, err := RewriteNamed(q, spans, func(name string) (int, error) {
		if n, ok := slots[name]; ok {
			return n, nil
		}
		order = append(order, name)
		n := len(order)
		slots[name] = n
		return n, nil
	})
	if err != nil {
		return "", nil, err
	}
	return rewritten, order, nil
}
