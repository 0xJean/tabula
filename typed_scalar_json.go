package tabula

import (
	"encoding/json"
	"fmt"
)

func jsonOperator[R any](result PGType[R], op string, document scalarOperand, operand scalarOperand, rhs string, both bool, nulls scalarNullRule) Expr[R] {
	n := scalarNode{syntax: scalarOperator, name: op, args: []scalarOperand{document, operand}, nulls: nulls}
	valid := nativeType(document.kind, "jsonb") || both && nativeType(document.kind, "json")
	if rhs == "text[]" {
		valid = valid && operand.kind.element != nil && nativeType(*operand.kind.element, "text")
	} else {
		valid = valid && nativeType(operand.kind, rhs)
	}
	if !valid {
		n.err = scalarError(op, "the declared JSON document and "+rhs+" operand", n.args)
	}
	return scalarResult(result, n)
}

func jsonResult(document scalarOperand) PGType[json.RawMessage] {
	name := "jsonb"
	if nativeType(document.kind, "json") {
		name = "json"
	}
	return scalarJSONKind(name)
}

// Computed documents use the same immutable canonical binding as pgtype's raw
// JSON mapping. This also protects manual and result-derived cursor values.
func scalarJSONKind(name string) PGType[json.RawMessage] {
	return CatalogType[json.RawMessage]("pg_catalog", name, nil).WithEncoder(func(value json.RawMessage) (any, error) {
		if value == nil {
			return nil, nil
		}
		if !json.Valid(value) {
			return nil, fmt.Errorf("tabula: invalid JSON document")
		}
		return string(value), nil
	})
}

// JSONGet extracts an object member as json/jsonb matching the input document.
// Missing members yield SQL NULL; a JSON null member remains a JSON value.
func JSONGet[J any, K ~string](document Expr[J], key Expr[K]) Expr[json.RawMessage] {
	return jsonOperator(jsonResult(document.scalarOperand()), "->", document.scalarOperand(), key.scalarOperand(), "text", true, nullAlways)
}

// JSONGetAt extracts an array element. Negative indexes count from the end.
func JSONGetAt[J any](document Expr[J], index Expr[int32]) Expr[json.RawMessage] {
	return jsonOperator(jsonResult(document.scalarOperand()), "->", document.scalarOperand(), index.scalarOperand(), "int4", true, nullAlways)
}

// JSONGetText extracts an object member as text. JSON null becomes SQL NULL.
func JSONGetText[J any, K ~string](document Expr[J], key Expr[K]) Expr[string] {
	return jsonOperator(Text(), "->>", document.scalarOperand(), key.scalarOperand(), "text", true, nullAlways)
}

// JSONGetTextAt extracts an array element as text; JSON null becomes SQL NULL.
func JSONGetTextAt[J any](document Expr[J], index Expr[int32]) Expr[string] {
	return jsonOperator(Text(), "->>", document.scalarOperand(), index.scalarOperand(), "int4", true, nullAlways)
}

// JSONGetPath traverses a text-array path, returning nullable json/jsonb.
func JSONGetPath[J any](document Expr[J], path Expr[[]string]) Expr[json.RawMessage] {
	return jsonOperator(jsonResult(document.scalarOperand()), "#>", document.scalarOperand(), path.scalarOperand(), "text[]", true, nullAlways)
}

// JSONGetTextPath traverses a text-array path, returning nullable text.
func JSONGetTextPath[J any](document Expr[J], path Expr[[]string]) Expr[string] {
	return jsonOperator(Text(), "#>>", document.scalarOperand(), path.scalarOperand(), "text[]", true, nullAlways)
}

// JSONHasKey tests for a top-level JSONB key or string array element.
func JSONHasKey[J any, K ~string](document Expr[J], key Expr[K]) Pred {
	return jsonOperator(Bool(), "?", document.scalarOperand(), key.scalarOperand(), "text", false, nullStrict)
}

// JSONContains tests JSONB containment with the native @> operator.
func JSONContains[A, B any](a Expr[A], b Expr[B]) Pred {
	return jsonOperator(Bool(), "@>", a.scalarOperand(), b.scalarOperand(), "jsonb", false, nullStrict)
}

// JSONContainedBy tests JSONB containment with the native <@ operator.
func JSONContainedBy[A, B any](a Expr[A], b Expr[B]) Pred {
	return jsonOperator(Bool(), "<@", a.scalarOperand(), b.scalarOperand(), "jsonb", false, nullStrict)
}

// JSONBCat concatenates JSONB using ||. The explicit result is a raw document,
// because native concatenation can change an object's or scalar's shape.
func JSONBCat[A, B any](a Expr[A], b Expr[B]) Expr[json.RawMessage] {
	return jsonOperator(scalarJSONKind("jsonb"), "||", a.scalarOperand(), b.scalarOperand(), "jsonb", false, nullStrict)
}

// JSONBDeleteKey deletes a top-level JSONB object key or string array element.
func JSONBDeleteKey[J any, K ~string](document Expr[J], key Expr[K]) Expr[json.RawMessage] {
	return jsonDelete("-", document, key.scalarOperand(), "text")
}

// JSONBDeleteKeys deletes the supplied top-level keys or string array elements.
func JSONBDeleteKeys[J any](document Expr[J], keys Expr[[]string]) Expr[json.RawMessage] {
	return jsonDelete("-", document, keys.scalarOperand(), "text[]")
}

// JSONBDeleteAt deletes an array element; negative indexes count from the end.
func JSONBDeleteAt[J any](document Expr[J], index Expr[int32]) Expr[json.RawMessage] {
	return jsonDelete("-", document, index.scalarOperand(), "int4")
}

// JSONBDeletePath deletes the value at a text-array path.
func JSONBDeletePath[J any](document Expr[J], path Expr[[]string]) Expr[json.RawMessage] {
	return jsonDelete("#-", document, path.scalarOperand(), "text[]")
}

func jsonDelete[J any](op string, document Expr[J], operand scalarOperand, rhs string) Expr[json.RawMessage] {
	return jsonOperator(scalarJSONKind("jsonb"), op, document.scalarOperand(), operand, rhs, false, nullStrict)
}

// JSONTypeOf returns the native JSON value kind. JSON null yields text "null";
// SQL NULL remains SQL NULL. Both json and jsonb are accepted.
func JSONTypeOf[J any](document Expr[J]) Expr[string] {
	name := "jsonb"
	if nativeType(document.kind.id, "json") {
		name = "json"
	}
	return checkedCall(Text(), name+"_typeof", nullStrict, expressionImmutable, []string{name}, document.scalarOperand())
}

// JSONMember retains a text key and a heterogeneous value for JSONBuildObject.
type JSONMember struct{ key, value scalarOperand }

// JSONPair declares an object member. A SQL NULL key causes the native database
// error; a SQL NULL value becomes JSON null.
func JSONPair[K ~string, V any](key Expr[K], value Expr[V]) JSONMember {
	return JSONMember{key.scalarOperand(), value.scalarOperand()}
}

// JSONBuildObject selects json_build_object or jsonb_build_object from result.
// JSON retains duplicate members; JSONB retains the last value for each key.
// Up to 50 ordered pairs are accepted. Values use reviewed built-in types.
func JSONBuildObject[R any](result PGType[R], pairs ...JSONMember) Expr[R] {
	name := result.id.name + "_build_object"
	n := scalarNode{name: name, nulls: nullNever, stability: expressionStable}
	if len(pairs) == 0 {
		n.functions = []string{"pg_catalog." + name + "()"}
	} else {
		n.functions = []string{"pg_catalog." + name + `("any")`}
	}
	valid := nativeType(result.id, "json", "jsonb") && len(pairs) <= 50
	for _, pair := range pairs {
		n.args = append(n.args, pair.key, pair.value)
		valid = valid && nativeType(pair.key.kind, "text") && reviewedType(pair.value.kind)
	}
	if !valid {
		n.err = scalarError("JSONBuildObject", "json/jsonb result and at most 50 text-key/reviewed-value pairs", n.args)
	}
	return scalarResult(result, n)
}
