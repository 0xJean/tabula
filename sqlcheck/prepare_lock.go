package sqlcheck

import (
	"errors"
	"fmt"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// The legacy zero-row probe may remove only complete trailing row-lock
// clauses. PREPARE and EXPLAIN always receive the original statement. Locks
// inside a query block require protocol description rather than SQL rewriting.
func lockProjectionQuery(query string) (string, error) {
	spans, err := pgstmt.ScanFragment(query)
	if err != nil {
		return "", err
	}
	var tokens []pgstmt.Span
	for _, token := range spans {
		if token.Kind != pgstmt.Comment {
			tokens = append(tokens, token)
		}
	}
	invalid := errors.New("expected a registered trailing row-lock clause with a quoted target")
	depth, start, head := 0, -1, ""
	for i, token := range tokens {
		if token.Kind == pgstmt.Symbol {
			if token.Text == "(" {
				depth++
			}
			if token.Text == ")" {
				depth--
			}
			continue
		}
		if token.Kind != pgstmt.Word {
			continue
		}
		if depth == 0 && head == "" {
			switch token.Text {
			case "select", "insert", "update", "delete":
				head = token.Text
			}
		}
		if token.Text != "for" {
			continue
		}
		if _, ok := lockStrengthEnd(tokens, i); !ok {
			continue
		}
		if depth != 0 {
			return "", fmt.Errorf("nested row locks: %w", ErrDescribeRequired)
		}
		start = i
		break
	}
	if start < 0 || head != "select" {
		return "", invalid
	}
	for i := start; i < len(tokens); {
		end, ok := lockStrengthEnd(tokens, i)
		if !ok || !wordSpan(tokens, end, "of") {
			return "", invalid
		}
		i = end + 1
		for {
			if i >= len(tokens) || tokens[i].Kind != pgstmt.Quoted {
				return "", invalid
			}
			i++
			if i >= len(tokens) || tokens[i].Text != "," {
				break
			}
			i++
		}
		if wordSpan(tokens, i, "nowait") {
			i++
		} else if wordSpan(tokens, i, "skip") && wordSpan(tokens, i+1, "locked") {
			i += 2
		}
	}
	return strings.TrimSpace(query[:tokens[start].Start]), nil
}

func lockStrengthEnd(tokens []pgstmt.Span, i int) (int, bool) {
	if !wordSpan(tokens, i, "for") {
		return 0, false
	}
	switch {
	case wordSpan(tokens, i+1, "update"), wordSpan(tokens, i+1, "share"):
		return i + 2, true
	case wordSpan(tokens, i+1, "key") && wordSpan(tokens, i+2, "share"):
		return i + 3, true
	case wordSpan(tokens, i+1, "no") && wordSpan(tokens, i+2, "key") && wordSpan(tokens, i+3, "update"):
		return i + 4, true
	default:
		return 0, false
	}
}
