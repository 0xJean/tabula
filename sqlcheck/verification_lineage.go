package sqlcheck

import (
	"github.com/0xJean/tabula/internal/pgstmt"
)

// Recognize only a complete, outer CAST target expression. A cast buried in
// CASE, arithmetic, a subquery, a string or a CTE is not lineage evidence.
func explicitResultCast(query string, index int) (string, bool) {
	spans, err := pgstmt.ScanFragment(query)
	if err != nil || index < 0 {
		return "", false
	}
	var tokens []pgstmt.Span
	for _, s := range spans {
		if s.Kind != pgstmt.Comment {
			tokens = append(tokens, s)
		}
	}
	depth, head, start := 0, "", -1
	for i, t := range tokens {
		if t.Kind == pgstmt.Symbol {
			if t.Text == "(" {
				depth++
			}
			if t.Text == ")" {
				depth--
			}
			continue
		}
		if depth != 0 || t.Kind != pgstmt.Word {
			continue
		}
		if head == "select" && (t.Text == "union" || t.Text == "intersect" || t.Text == "except") {
			// Set resolution may coerce a domain cast in one arm to its base.
			return "", false
		}
		if head == "" {
			switch t.Text {
			case "select", "insert", "update", "delete":
				head = t.Text
				if head == "select" {
					start = i + 1
				}
			}
		} else if head != "select" && t.Text == "returning" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", false
	}
	if wordSpan(tokens, start, "distinct") {
		start++
		if wordSpan(tokens, start, "on") {
			start++
			if start >= len(tokens) || tokens[start].Text != "(" {
				return "", false
			}
			start = spanGroupEnd(tokens, start) + 1
		}
	}
	if start < 0 || start >= len(tokens) {
		return "", false
	}
	depth = 0
	current := 0
	end := len(tokens)
	for i := start; i < len(tokens); i++ {
		t := tokens[i]
		if depth == 0 {
			if t.Kind == pgstmt.Symbol && t.Text == "*" {
				// An expanded projection has no fixed syntax-to-result index.
				return "", false
			}
			if t.Kind == pgstmt.Symbol && t.Text == "," {
				if current == index {
					end = i
					break
				}
				current++
				start = i + 1
				continue
			}
			if t.Kind == pgstmt.Word {
				switch t.Text {
				case "from", "where", "group", "having", "window", "order", "limit", "offset", "fetch", "for", "union", "intersect", "except":
					end = i
					i = len(tokens)
					continue
				}
			}
		}
		if t.Kind == pgstmt.Symbol {
			if t.Text == "(" {
				depth++
			}
			if t.Text == ")" {
				depth--
			}
		}
	}
	if current != index || end <= start {
		return "", false
	}
	value := tokens[start:end]
	if !wordSpan(value, 0, "cast") || len(value) < 7 || value[1].Text != "(" {
		return "", false
	}
	endCast := spanGroupEnd(value, 1)
	if endCast < 0 {
		return "", false
	}
	if endCast != len(value)-1 {
		if endCast != len(value)-3 || !wordSpan(value, endCast+1, "as") ||
			value[endCast+2].Kind != pgstmt.Word && value[endCast+2].Kind != pgstmt.Quoted {
			return "", false
		}
	}
	depth = 0
	target := -1
	for i := 2; i < endCast; i++ {
		t := value[i]
		if t.Kind == pgstmt.Symbol {
			if t.Text == "(" {
				depth++
			}
			if t.Text == ")" {
				depth--
			}
		}
		if depth == 0 && wordSpan(value, i, "as") {
			target = i + 1
		}
	}
	if target < 0 || endCast-target != 3 {
		return "", false
	}
	for _, i := range []int{target, target + 2} {
		if value[i].Kind != pgstmt.Word && value[i].Kind != pgstmt.Quoted {
			return "", false
		}
	}
	if value[target+1].Text != "." {
		return "", false
	}
	return query[value[target].Start:value[target+2].End], true
}

func wordSpan(tokens []pgstmt.Span, i int, word string) bool {
	return i >= 0 && i < len(tokens) && tokens[i].Kind == pgstmt.Word && tokens[i].Text == word
}
func spanGroupEnd(tokens []pgstmt.Span, start int) int {
	depth := 0
	for i := start; i < len(tokens); i++ {
		if tokens[i].Kind != pgstmt.Symbol {
			continue
		}
		switch tokens[i].Text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
