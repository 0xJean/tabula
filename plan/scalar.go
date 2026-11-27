package plan

import (
	"github.com/0xJean/tabula"

	"fmt"
	"slices"
	"strings"
)

// The grammar and native names are selected exclusively by package constructors.
// Arguments always retain their catalog identities and declaration trees.
type scalarSyntax uint8

const (
	scalarCall scalarSyntax = iota
	scalarOperator
	scalarCast
	scalarKeyword
	scalarNull
	scalarCase
	scalarArray
	scalarAll
	scalarConditional
)

type scalarNullRule uint8

const (
	nullStrict scalarNullRule = iota
	nullNever
	nullAlways
	nullAll
	nullFirst
	nullCase
)

type scalarNode struct {
	syntax    scalarSyntax
	name      string
	result    catalogType
	args      []scalarOperand
	functions []string
	stability expressionStability
	nulls     scalarNullRule
	err       error
}

func (n scalarNode) expressionTraits() expressionTraits {
	children := make([]expressionPart, len(n.args))
	var identity strings.Builder
	fmt.Fprintf(&identity, "scalar/%d/%s/%s/%d/%d", n.syntax, n.name, n.result.Qualified(), n.stability, n.nulls)
	for i, arg := range n.args {
		children[i] = arg.part
		identity.WriteString("/" + arg.kind.Qualified())
	}
	return expressionTraits{identity: identity.String(), children: children, stability: n.stability}
}

func (n scalarNode) nullableAfterJoin(outer map[*occurrence]bool) bool {
	null := func(v scalarOperand) bool { return v.nullableWith(outer) }
	switch n.nulls {
	case nullNever:
		return false
	case nullAlways:
		return true
	case nullAll:
		return !slices.ContainsFunc(n.args, func(v scalarOperand) bool { return !null(v) })
	case nullFirst:
		return len(n.args) == 0 || null(n.args[0])
	case nullCase:
		for i := 1; i < len(n.args); i += 2 {
			if null(n.args[i]) {
				return true
			}
		}
		return len(n.args)%2 == 0 || null(n.args[len(n.args)-1])
	default:
		return slices.ContainsFunc(n.args, null)
	}
}

func (n scalarNode) render(c *typedCompiler) (string, error) {
	c.extendedForm = true
	if n.err != nil {
		return "", n.err
	}
	if n.result.GoType == nil {
		return "", fmt.Errorf("tabula: %s requires a declared result type", n.name)
	}
	parts := make([]string, len(n.args))
	for i, arg := range n.args {
		if arg.part == nil || arg.kind.GoType == nil {
			return "", fmt.Errorf("tabula: %s operand %d is undeclared", n.name, i+1)
		}
		var err error
		parts[i], err = arg.part.render(c)
		if err != nil {
			return "", fmt.Errorf("tabula: %s operand %d: %w", n.name, i+1, err)
		}
	}
	for _, fn := range n.functions {
		c.requireNative(fn)
	}
	if n.syntax == scalarCast {
		for _, fn := range scalarCastFunctions(n.args[0].kind, n.result) {
			c.requireNative(fn)
		}
	}
	switch n.syntax {
	case scalarOperator:
		return "(" + parts[0] + " OPERATOR(pg_catalog." + n.name + ") " + parts[1] + ")", nil
	case scalarCast:
		return n.result.Cast(parts[0]), nil
	case scalarKeyword:
		return n.name, nil
	case scalarNull:
		return n.result.Cast("NULL"), nil
	case scalarCase:
		var out strings.Builder
		out.WriteString("(CASE")
		for i := 0; i+1 < len(parts); i += 2 {
			out.WriteString(" WHEN " + parts[i] + " THEN " + parts[i+1])
		}
		if len(parts)%2 != 0 {
			out.WriteString(" ELSE " + parts[len(parts)-1])
		}
		return out.String() + " END)", nil
	case scalarArray:
		return n.result.Cast("ARRAY[" + strings.Join(parts, ", ") + "]"), nil
	case scalarAll:
		return "(" + parts[0] + " OPERATOR(pg_catalog.<>) ALL(" + parts[1] + "))", nil
	case scalarConditional:
		if n.name == "NULLIF" {
			// Keep scalar operands in this SELECT's evaluation/aggregate level.
			// Removing the only equal element yields NULL through the subscript.
			if n.result.Element == nil {
				return "(pg_catalog.array_remove(ARRAY[" + parts[0] + "], " + parts[1] + "))[1]", nil
			}
			return "(CASE WHEN " + parts[0] + " OPERATOR(pg_catalog.=) " + parts[1] + " THEN " + n.result.Cast("NULL") + " ELSE " + parts[0] + " END)", nil
		}
		return n.name + "(" + strings.Join(parts, ", ") + ")", nil
	default:
		return "pg_catalog." + typedQuote(n.name) + "(" + strings.Join(parts, ", ") + ")", nil
	}
}

func scalarResult[V any](kind tabula.PGType[V], node scalarNode) Expr[V] {
	node.result = typeID(kind)
	if node.err == nil {
		switch node.syntax {
		case scalarOperator:
			fn := scalarOperatorFunction(node.name, node.args[0].kind, node.args[1].kind)
			if fn == "" {
				node.err = scalarError(node.name, "a reviewed pg_catalog operator", node.args)
			} else {
				node.functions = append(node.functions, fn)
			}
		case scalarAll:
			if node.args[1].kind.Element != nil {
				fn := scalarOperatorFunction("<>", node.args[0].kind, *node.args[1].kind.Element)
				if fn == "" {
					node.err = scalarError(node.name, "a reviewed pg_catalog inequality", node.args)
				} else {
					node.functions = append(node.functions, fn)
				}
			}
		case scalarConditional:
			if node.name == "NULLIF" {
				fn := "pg_catalog.array_remove(anycompatiblearray,anycompatible)"
				if typeID(kind).Element != nil {
					fn = scalarOperatorFunction("=", node.args[0].kind, node.args[1].kind)
				}
				if fn == "" {
					node.err = scalarError(node.name, "a reviewed pg_catalog equality", node.args)
				} else {
					node.functions = append(node.functions, fn)
				}
			}
		}
	}
	return Expr[V]{part: node, kind: kind, nullable: node.nullableAfterJoin(nil)}
}

func scalarOperatorFunction(op string, a, b catalogType) string {
	left, right := a.Name, b.Name
	if a.Element != nil && b.Element != nil {
		left, right = "anyarray", "anyarray"
		if op == "||" {
			left, right = "anycompatiblearray", "anycompatiblearray"
		}
	} else if b.Element != nil && nativeType(*b.Element, "text") {
		right = "text[]"
	}
	// varchar comparisons resolve through the binary-compatible text type.
	if left == "varchar" {
		left = "text"
	}
	if right == "varchar" {
		right = "text"
	}
	if left == "cidr" {
		left = "inet"
	}
	if right == "cidr" {
		right = "inet"
	}
	return scalarOperators[left+"/"+op+"/"+right]
}

func scalarArgs(values ...Scalar) []scalarOperand {
	args := make([]scalarOperand, len(values))
	for i, value := range values {
		if !isNilBound(value) {
			args[i] = value.scalarOperand()
		}
	}
	return args
}

func scalarOperands[V any](first Expr[V], rest []Expr[V]) []scalarOperand {
	args := []scalarOperand{first.scalarOperand()}
	for _, v := range rest {
		args = append(args, v.scalarOperand())
	}
	return args
}

func scalarError(name, expected string, args []scalarOperand) error {
	types := make([]string, len(args))
	for i, a := range args {
		types[i] = a.kind.Qualified()
	}
	return fmt.Errorf("tabula: %s requires %s; supplied (%s)", name, expected, strings.Join(types, ", "))
}

func nativeType(t catalogType, names ...string) bool {
	return t.GoType != nil && t.Element == nil && t.Schema == "pg_catalog" && slices.Contains(names, t.Name)
}

func reviewedType(t catalogType) bool {
	if t.Element != nil {
		return t.Element.Element == nil && reviewedType(*t.Element)
	}
	return nativeType(t, "bool", "int2", "int4", "int8", "float4", "float8", "numeric", "oid", "text", "varchar", "bpchar",
		"bytea", "uuid", "json", "jsonb", "date", "time", "timetz", "timestamp", "timestamptz", "interval", "inet", "cidr")
}

func comparableScalar(t catalogType, ordered bool) bool {
	if !reviewedType(t) {
		return false
	}
	if t.Element != nil {
		return comparableScalar(*t.Element, ordered)
	}
	return t.Name != "json"
}

// TypedNull declares a SQL NULL with an exact catalog type, without an input.
func TypedNull[V any](kind tabula.PGType[V]) Expr[V] {
	return scalarResult(kind, scalarNode{syntax: scalarNull, name: "NULL", nulls: nullAlways})
}
