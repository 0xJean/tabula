package tabula

import (
	"fmt"
	"strings"
)

// In tests membership in a nonempty, fixed list of typed expressions. Use Any
// with an array input when the list length varies between calls.
func In[V any](value, first Expr[V], rest ...Expr[V]) Pred {
	list := membership{value: value.part, valid: sameSQLType(value.kind.id, first.kind.id), members: []expressionPart{first.part}}
	nullable := value.nullable || first.nullable
	for _, item := range rest {
		list.valid = list.valid && sameSQLType(value.kind.id, item.kind.id)
		list.members = append(list.members, item.part)
		nullable = nullable || item.nullable
	}
	return Pred{list, Bool(), nullable}
}

// Any tests equality with any element of a PostgreSQL array declared with
// ArrayOf. An empty array yields false and a NULL array yields NULL.
func Any[V any](value Expr[V], array Expr[[]V]) Pred {
	element := array.kind.id.element
	return Pred{membership{value: value.part, members: []expressionPart{array.part}, array: true,
		valid: element != nil && sameSQLType(value.kind.id, *element)}, Bool(), true}
}

type membership struct {
	value   expressionPart
	members []expressionPart
	array   bool
	valid   bool
}

func (v membership) render(c *typedCompiler) (string, error) {
	if !v.valid || v.value == nil {
		return "", fmt.Errorf("tabula: membership requires the declared element type on both sides")
	}
	left, err := v.value.render(c)
	if err != nil {
		return "", err
	}
	items := make([]string, len(v.members))
	for i, member := range v.members {
		if member == nil {
			return "", fmt.Errorf("tabula: undeclared membership operand")
		}
		items[i], err = member.render(c)
		if err != nil {
			return "", err
		}
	}
	if v.array {
		return "(" + left + " = ANY(" + items[0] + "))", nil
	}
	return "(" + left + " IN (" + strings.Join(items, ", ") + "))", nil
}

// Like matches a PostgreSQL string pattern. The pattern retains the database's
// wildcard and escape semantics. Bind a pattern input with BindValue.
func Like[V, P ~string](value Expr[V], pattern Expr[P]) Pred {
	return matchPattern("LIKE", value, pattern)
}

// ILike matches a string pattern case-insensitively using PostgreSQL's locale.
func ILike[V, P ~string](value Expr[V], pattern Expr[P]) Pred {
	return matchPattern("ILIKE", value, pattern)
}

func matchPattern[V, P ~string](op string, value Expr[V], pattern Expr[P]) Pred {
	isText := func(t catalogType) bool {
		return t.schema == "pg_catalog" && (t.name == "text" || t.name == "varchar" || t.name == "bpchar")
	}
	return Pred{comparison{op, value.part, pattern.part, isText(value.kind.id) && isText(pattern.kind.id)}, Bool(), value.nullable || pattern.nullable}
}
