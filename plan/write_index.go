package plan

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// IndexPredicate is a sealed schema predicate used exclusively for ON CONFLICT
// index inference. Its values are fixed schema facts, never runtime inputs.
// Only reviewed native integer, boolean and text comparisons are accepted.
type IndexPredicate struct{ node indexPredicateNode }

type indexPredicateNode interface {
	renderIndex(*typedCompiler, *occurrence) (string, error)
}

type indexTest struct {
	field   *fieldRef
	op      string
	literal string
	err     error
}

// IndexEq compares a target field to a fixed schema value for index inference.
func IndexEq[V any](field Field[V], schemaValue V) IndexPredicate {
	return indexCompare(field, schemaValue, "=")
}

// IndexNe compares a target field to a fixed schema value for index inference.
func IndexNe[V any](field Field[V], schemaValue V) IndexPredicate {
	return indexCompare(field, schemaValue, "<>")
}

// IndexLt compares a target field to a fixed schema value for index inference.
func IndexLt[V any](field Field[V], schemaValue V) IndexPredicate {
	return indexCompare(field, schemaValue, "<")
}

// IndexLe compares a target field to a fixed schema value for index inference.
func IndexLe[V any](field Field[V], schemaValue V) IndexPredicate {
	return indexCompare(field, schemaValue, "<=")
}

// IndexGt compares a target field to a fixed schema value for index inference.
func IndexGt[V any](field Field[V], schemaValue V) IndexPredicate {
	return indexCompare(field, schemaValue, ">")
}

// IndexGe compares a target field to a fixed schema value for index inference.
func IndexGe[V any](field Field[V], schemaValue V) IndexPredicate {
	return indexCompare(field, schemaValue, ">=")
}

// IndexTrue tests a boolean target column for TRUE in index inference.
func IndexTrue(field Field[bool]) IndexPredicate {
	return IndexPredicate{indexTest{field: field.ref, op: "IS TRUE"}}
}

// IndexFalse tests a boolean target column for FALSE in index inference.
func IndexFalse(field Field[bool]) IndexPredicate {
	return IndexPredicate{indexTest{field: field.ref, op: "IS FALSE"}}
}

// IndexNull tests a target column for SQL NULL in index inference.
func IndexNull[V any](field Field[V]) IndexPredicate {
	return IndexPredicate{indexTest{field: field.ref, op: "IS NULL"}}
}

// IndexNotNull tests a target column for a non-NULL value in index inference.
func IndexNotNull[V any](field Field[V]) IndexPredicate {
	return IndexPredicate{indexTest{field: field.ref, op: "IS NOT NULL"}}
}

type indexBoolean struct {
	op    string
	terms []IndexPredicate
}

// IndexAnd combines schema predicates with SQL AND.
func IndexAnd(first, second IndexPredicate, rest ...IndexPredicate) IndexPredicate {
	return IndexPredicate{indexBoolean{"AND", slices.Concat([]IndexPredicate{first, second}, rest)}}
}

// IndexOr combines schema predicates with SQL OR.
func IndexOr(first, second IndexPredicate, rest ...IndexPredicate) IndexPredicate {
	return IndexPredicate{indexBoolean{"OR", slices.Concat([]IndexPredicate{first, second}, rest)}}
}

// IndexNot negates one schema predicate with SQL NOT.
func IndexNot(predicate IndexPredicate) IndexPredicate {
	return IndexPredicate{indexBoolean{"NOT", []IndexPredicate{predicate}}}
}

func indexCompare[V any](field Field[V], value V, op string) IndexPredicate {
	n := indexTest{field: field.ref, op: op}
	n.literal, n.err = indexLiteral(typeID(field.kind), value)
	return IndexPredicate{n}
}

func indexLiteral(kind catalogType, value any) (string, error) {
	if kind.Encoder != nil || kind.Element != nil || kind.Schema != "pg_catalog" {
		return "", fmt.Errorf("tabula: index schema values require a reviewed native type without an encoder")
	}
	v := reflect.ValueOf(value)
	if !v.IsValid() || v.Type() != kind.GoType {
		return "", fmt.Errorf("tabula: incompatible index schema value")
	}
	var literal string
	switch kind.Name {
	case "int2", "int4", "int8":
		if v.Kind() < reflect.Int || v.Kind() > reflect.Int64 {
			break
		}
		value := v.Int()
		if kind.Name == "int2" && (value < -32768 || value > 32767) ||
			kind.Name == "int4" && (value < -2147483648 || value > 2147483647) {
			break
		}
		literal = strconv.FormatInt(value, 10)
	case "bool":
		if v.Kind() == reflect.Bool {
			literal = strings.ToUpper(strconv.FormatBool(v.Bool()))
		}
	case "text", "varchar":
		if v.Kind() == reflect.String && utf8.ValidString(v.String()) && !strings.ContainsRune(v.String(), 0) {
			// An explicit escape string is independent of standard_conforming_strings.
			literal = "E'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(v.String()) + "'"
		}
	}
	if literal == "" {
		return "", fmt.Errorf("tabula: unsupported index schema value type or range")
	}
	return kind.Cast(literal), nil
}

func (n indexTest) renderIndex(c *typedCompiler, target *occurrence) (string, error) {
	if n.err != nil {
		return "", n.err
	}
	f := n.field
	if f == nil || f.source != target || f.secret {
		return "", fmt.Errorf("tabula: index predicate requires a non-secret column of the insertion target")
	}
	if (n.op == "IS TRUE" || n.op == "IS FALSE") && !nativeType(f.kind, "bool") {
		return "", fmt.Errorf("tabula: index boolean test requires pg_catalog.bool")
	}
	if err := c.read(f); err != nil {
		return "", err
	}
	column := typedQuote(f.name)
	if n.literal == "" {
		return "(" + column + " " + n.op + ")", nil
	}
	operations := map[string]string{"=": "eq", "<>": "ne", "<": "lt", "<=": "le", ">": "gt", ">=": "ge"}
	if function := scalarOperatorFunction(n.op, f.kind, f.kind); function != "" {
		c.requireNative(function)
	} else if err := requireReadComparison(c, f.kind, operations[n.op]); err != nil {
		return "", err
	}
	return "(" + column + " OPERATOR(pg_catalog." + n.op + ") " + n.literal + ")", nil
}

func (n indexBoolean) renderIndex(c *typedCompiler, target *occurrence) (string, error) {
	parts := make([]string, len(n.terms))
	for i, term := range n.terms {
		if term.node == nil {
			return "", fmt.Errorf("tabula: undeclared index predicate")
		}
		var err error
		parts[i], err = term.node.renderIndex(c, target)
		if err != nil {
			return "", err
		}
	}
	if n.op == "NOT" {
		return "(NOT " + parts[0] + ")", nil
	}
	return "(" + strings.Join(parts, " "+n.op+" ") + ")", nil
}
