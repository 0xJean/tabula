package sqlcheck

import (
	"fmt"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// Qualified type names are resolved by catalog identity, independent of the
// checking connection's USAGE on the routine or argument type's schema.
// PostgreSQL handles unqualified SQL type spellings such as "double precision".
type readinessType struct {
	Spelling, Schema, Name string
	Array                  bool
}

func resolveReadinessItem(m MissingRequirement) (readinessItem, error) {
	item := readinessItem{MissingRequirement: m}
	if !catalogName(m.Object) {
		return item, fmt.Errorf("sqlcheck: invalid %s name %q", m.Kind, m.Object)
	}
	if m.Kind == "SCHEMA" {
		item.Schema = m.Object
		return item, nil
	}
	tokens, err := pgstmt.ScanFragment(m.Object)
	if err != nil {
		return item, fmt.Errorf("sqlcheck: invalid %s %q: %w", m.Kind, m.Object, err)
	}
	schema, name, ok := readinessQualifiedName(tokens)
	if !ok {
		return item, fmt.Errorf("sqlcheck: expected schema-qualified %s name, got %q", m.Kind, m.Object)
	}
	item.Schema, item.Name = schema, name
	if m.Kind == "FUNCTION" {
		item.Args, err = readinessArgs(m.Object, tokens[3:])
		return item, err
	}
	if len(tokens) != 3 {
		return item, fmt.Errorf("sqlcheck: invalid relation name %q", m.Object)
	}
	return item, nil
}

func readinessQualifiedName(tokens []pgstmt.Span) (schema, name string, ok bool) {
	if len(tokens) < 3 || tokens[1].Text != "." {
		return "", "", false
	}
	ident := func(token pgstmt.Span) string {
		switch token.Kind {
		case pgstmt.Word:
			return token.Text
		case pgstmt.Quoted:
			return strings.ReplaceAll(token.Text[1:len(token.Text)-1], `""`, `"`)
		default:
			return ""
		}
	}
	schema, name = ident(tokens[0]), ident(tokens[2])
	return schema, name, catalogName(schema) && catalogName(name)
}

func readinessArgs(signature string, tokens []pgstmt.Span) ([]readinessType, error) {
	bad := fmt.Errorf("sqlcheck: invalid function signature %q", signature)
	if len(tokens) < 2 || tokens[0].Text != "(" || tokens[len(tokens)-1].Text != ")" {
		return nil, bad
	}
	tokens = tokens[1 : len(tokens)-1]
	args := []readinessType{}
	for len(tokens) > 0 {
		end, depth := 0, 0
		for end < len(tokens) {
			t := tokens[end]
			if t.Text == "," && depth == 0 {
				break
			}
			switch t.Text {
			case "(":
				depth++
			case ")":
				depth--
			}
			if depth < 0 || t.Kind == pgstmt.Comment || t.Kind == pgstmt.Literal || t.Kind == pgstmt.Param {
				return nil, bad
			}
			end++
		}
		if end == 0 || depth != 0 || end == len(tokens)-1 {
			return nil, bad
		}
		arg := tokens[:end]
		typ := readinessType{Spelling: signature[arg[0].Start:arg[len(arg)-1].End]}
		if schema, name, ok := readinessQualifiedName(arg); ok {
			typ.Schema, typ.Name = schema, name
			// A schema-qualified catalog type name optionally takes array suffixes.
			// Dimensions do not change a PostgreSQL array type's identity.
			for rest := arg[3:]; len(rest) > 0; rest = rest[2:] {
				if len(rest) < 2 || rest[0].Text != "[" || rest[1].Text != "]" {
					return nil, bad
				}
				typ.Array = true
			}
		}
		args = append(args, typ)
		if end == len(tokens) {
			break
		}
		tokens = tokens[end+1:]
	}
	return args, nil
}
