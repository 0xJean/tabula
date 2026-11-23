package tabula

import (
	"fmt"
	"maps"
	"strings"
)

// OrderSpec resolves caller-supplied sort field and direction against a fixed
// whitelist and returns a safe SQL ORDER BY expression for Listing.Order.
type OrderSpec struct {
	Columns          map[string]string
	DefaultColumn    string
	DefaultDirection string
	TieBreaker       string
}

// MustOrderSpec validates spec and returns an immutable copy. Any malformed
// specification panics at package initialization.
func MustOrderSpec(spec OrderSpec) OrderSpec {
	col := strings.TrimSpace(spec.DefaultColumn)
	if !safeOrderExpr(col) {
		panic(fmt.Sprintf("tabula.MustOrderSpec: invalid default column %q", spec.DefaultColumn))
	}
	dir := strings.ToUpper(strings.TrimSpace(spec.DefaultDirection))
	if dir == "" {
		dir = "DESC"
	}
	if dir != "ASC" && dir != "DESC" {
		panic(fmt.Sprintf("tabula.MustOrderSpec: invalid default direction %q", spec.DefaultDirection))
	}
	tie := strings.TrimSpace(spec.TieBreaker)
	if tie != "" && !safeOrderExpr(tie) {
		panic(fmt.Sprintf("tabula.MustOrderSpec: invalid tie-breaker %q", spec.TieBreaker))
	}
	cols := make(map[string]string, len(spec.Columns))
	for k, v := range spec.Columns {
		key, val := strings.TrimSpace(k), strings.TrimSpace(v)
		if key == "" || !safeOrderExpr(val) {
			panic(fmt.Sprintf("tabula.MustOrderSpec: invalid mapping %q -> %q", k, v))
		}
		cols[key] = val
	}
	return OrderSpec{
		Columns:          maps.Clone(cols),
		DefaultColumn:    col,
		DefaultDirection: dir,
		TieBreaker:       tie,
	}
}

// Resolve maps caller-provided sort key and direction to a safe SQL ORDER BY
// expression (without leading "ORDER BY").
func (s OrderSpec) Resolve(by, dir string) string {
	column := s.DefaultColumn
	if mapped, ok := s.Columns[strings.TrimSpace(by)]; ok {
		column = mapped
	}
	direction := s.DefaultDirection
	if direction == "" {
		direction = "DESC"
	}
	switch strings.ToUpper(strings.TrimSpace(dir)) {
	case "ASC":
		direction = "ASC"
	case "DESC":
		direction = "DESC"
	}
	out := column + " " + direction
	if s.TieBreaker != "" && !sameLeadingColumn(column, s.TieBreaker) {
		out += ", " + s.TieBreaker
	}
	return out
}

func sameLeadingColumn(primary, tieBreaker string) bool {
	lead, _, _ := strings.Cut(strings.TrimSpace(tieBreaker), " ")
	return strings.EqualFold(strings.TrimSpace(primary), lead)
}

func safeOrderExpr(expr string) bool {
	if expr == "" {
		return false
	}
	if strings.ContainsAny(expr, ";'\"") || strings.Contains(expr, "--") || strings.Contains(expr, "/*") {
		return false
	}
	for _, r := range expr {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '.', r == ' ', r == ',':
		default:
			return false
		}
	}
	return true
}
