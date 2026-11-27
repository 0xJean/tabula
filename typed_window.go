package tabula

import (
	"fmt"
	"slices"
	"strings"
)

// WindowSpec declares partitioning, peer order and an optional frame. A zero
// Frame retains PostgreSQL's default RANGE frame. Name optionally shares this
// complete specification within the SELECT; repeated names must agree exactly.
type WindowSpec struct {
	Name      string
	Partition []Scalar
	Order     []RowTerm
	Frame     WindowFrame
}

// RowNumber numbers rows from one within each partition. Use Over to supply
// the partition and order; a row number alone is not a result-key proof.
func RowNumber() Expr[int64] { return newWindowFunction("row_number", Int8()) }

// Rank ranks peer groups with gaps, starting at one. It requires Over.
func Rank() Expr[int64] { return newWindowFunction("rank", Int8()) }

// DenseRank ranks peer groups without gaps, starting at one. It requires Over.
func DenseRank() Expr[int64] { return newWindowFunction("dense_rank", Int8()) }

// PercentRank returns (rank-1)/(partition size-1), or zero for a singleton.
func PercentRank() Expr[float64] {
	return newWindowFunction("percent_rank", CatalogType[float64]("pg_catalog", "float8", nil))
}

// CumeDist returns the fraction of partition rows preceding or peer to a row.
func CumeDist() Expr[float64] {
	return newWindowFunction("cume_dist", CatalogType[float64]("pg_catalog", "float8", nil))
}

// NTile divides the partition into the supplied positive int4 bucket count.
// A NULL count produces NULL; a nonpositive runtime count retains SQLSTATE 22014.
func NTile(buckets Expr[int32]) Expr[int32] {
	return newWindowFunction("ntile", Int4(), buckets.scalarOperand())
}

// Lag reads value at the explicit int4 offset before this row, or fallback
// when no such row exists. SQL NULL offsets and values retain native semantics.
func Lag[V any](value Expr[V], offset Expr[int32], fallback Expr[V]) Expr[V] {
	return newWindowFunction("lag", value.kind, scalarArgs(value, offset, fallback)...)
}

// Lead reads value at the explicit int4 offset after this row, or fallback
// when no such row exists. PostgreSQL RESPECT NULLS semantics apply.
func Lead[V any](value Expr[V], offset Expr[int32], fallback Expr[V]) Expr[V] {
	return newWindowFunction("lead", value.kind, scalarArgs(value, offset, fallback)...)
}

// FirstValue reads the first frame row, or NULL for an empty frame.
func FirstValue[V any](value Expr[V]) Expr[V] {
	return newWindowFunction("first_value", value.kind, value.scalarOperand())
}

// LastValue reads the last frame row, or NULL for an empty frame. The default
// frame ends at the last peer of the current row, not the partition's last row.
func LastValue[V any](value Expr[V]) Expr[V] {
	return newWindowFunction("last_value", value.kind, value.scalarOperand())
}

// NthValue reads the one-based int4 position in the frame, or NULL if absent.
// A nonpositive runtime position retains PostgreSQL SQLSTATE 22016.
func NthValue[V any](value Expr[V], position Expr[int32]) Expr[V] {
	return newWindowFunction("nth_value", value.kind, scalarArgs(value, position)...)
}

type windowFunction struct {
	name   string
	result catalogType
	args   []scalarOperand
}

func newWindowFunction[V any](name string, kind PGType[V], args ...scalarOperand) Expr[V] {
	n := windowFunction{name: name, result: kind.id, args: args}
	return Expr[V]{part: n, kind: kind, nullable: n.nullableAfterJoin(nil)}
}

func (n windowFunction) expressionTraits() expressionTraits {
	t := expressionTraits{identity: "window-function/" + n.name + "/" + n.result.qualified(), window: true}
	for _, arg := range n.args {
		t.children = append(t.children, arg.part)
		t.identity += "/" + arg.kind.qualified()
	}
	return t
}

func (n windowFunction) nullableAfterJoin(outer map[*occurrence]bool) bool {
	switch n.name {
	case "row_number", "rank", "dense_rank", "percent_rank", "cume_dist":
		return false
	case "ntile", "lag", "lead":
		return slices.ContainsFunc(n.args, func(arg scalarOperand) bool { return arg.nullableWith(outer) })
	default:
		return true
	}
}

func (windowFunction) render(*typedCompiler) (string, error) {
	return "", fmt.Errorf("tabula: window function requires Over")
}

func (n windowFunction) renderOver(c *typedCompiler) (string, error) {
	if n.result.goType == nil {
		return "", fmt.Errorf("tabula: window function requires a declared result type")
	}
	signature := "pg_catalog." + n.name + "()"
	switch n.name {
	case "ntile":
		if !nativeType(n.args[0].kind, "int4") {
			return "", fmt.Errorf("tabula: ntile requires int4 buckets")
		}
		signature = "pg_catalog.ntile(integer)"
	case "lag", "lead":
		if !nativeType(n.args[1].kind, "int4") || !sameSQLType(n.args[0].kind, n.args[2].kind) || n.args[0].kind.goType != n.args[2].kind.goType {
			return "", fmt.Errorf("tabula: %s requires an int4 offset and an exactly matching default", n.name)
		}
		signature = "pg_catalog." + n.name + "(anycompatible,integer,anycompatible)"
	case "first_value", "last_value":
		signature = "pg_catalog." + n.name + "(anyelement)"
	case "nth_value":
		if !nativeType(n.args[1].kind, "int4") {
			return "", fmt.Errorf("tabula: nth_value requires an int4 position")
		}
		signature = "pg_catalog.nth_value(anyelement,integer)"
	}
	parts := make([]string, len(n.args))
	for i, arg := range n.args {
		if arg.part == nil || arg.kind.goType == nil {
			return "", fmt.Errorf("tabula: %s requires declared arguments", n.name)
		}
		var err error
		parts[i], err = arg.part.render(c)
		if err != nil {
			return "", err
		}
	}
	c.requireNative(signature)
	return "pg_catalog." + n.name + "(" + strings.Join(parts, ", ") + ")", nil
}

// Over applies a window to a ranking/value function or an ordinary aggregate.
// Aggregate FILTER is supported; DISTINCT and aggregate-local ORDER BY are not
// supported by PostgreSQL window aggregates. Window results are legal only in
// a SELECT projection and its final ORDER BY; filter an exported result outside
// the query that computes it. Every supplied slice is copied.
func Over[V any](function Expr[V], spec WindowSpec) Expr[V] {
	spec.Partition = slices.Clone(spec.Partition)
	spec.Order = slices.Clone(spec.Order)
	n := windowExpression{function: function.part, spec: spec}
	if root, ok := function.part.(aggregateRoot); ok {
		a := root.aggregateRoot()
		a.legacy = false
		n.function = a
	}
	function.part = n
	function.nullable = n.nullableAfterJoin(nil)
	return function
}

type windowExpression struct {
	function expressionPart
	spec     WindowSpec
}

func (n windowExpression) expressionTraits() expressionTraits {
	f, _ := expressionStructure(n.function)
	t := expressionTraits{identity: "over/" + f.identity + "/" + n.spec.Name, children: slices.Clone(f.children), window: true, stability: f.stability}
	if a, ok := n.function.(aggregateCall); ok && (a.fn == "array_agg" || a.fn == "string_agg") && len(n.spec.Order) > 0 {
		t.stability = expressionImmutable
	}
	if n.spec.Frame.Mode == FrameRange && len(n.spec.Order) == 1 && nativeType(n.spec.Order[0].orderValue().kind, "timestamptz") &&
		(n.spec.Frame.Start.value.part != nil || n.spec.Frame.End.value.part != nil) {
		t.stability = expressionStable // in_range(timestamptz,...,interval) uses TimeZone.
	}
	for _, p := range scalarArgs(n.spec.Partition...) {
		t.children = append(t.children, p.part)
		t.identity += "/partition/" + p.kind.qualified()
	}
	for _, term := range n.spec.Order {
		t.children = append(t.children, term.orderValue().part)
		t.identity += fmt.Sprintf("/order/%t/%t", term.desc, term.nullsFirst())
	}
	t.identity += n.spec.Frame.identity()
	for _, bound := range []FrameBound{n.spec.Frame.Start, n.spec.Frame.End} {
		if bound.value.part != nil {
			t.children = append(t.children, bound.value.part)
		}
	}
	return t
}

func (n windowExpression) nullableAfterJoin(outer map[*occurrence]bool) bool {
	if f, ok := n.function.(describedExpression); ok {
		return f.nullableAfterJoin(outer)
	}
	return true
}

func (n windowExpression) render(c *typedCompiler) (string, error) {
	c.extendedForm = true
	if err := n.validate(nil); err != nil {
		return "", err
	}
	var function string
	var err error
	switch f := n.function.(type) {
	case windowFunction:
		function, err = f.renderOver(c)
	case aggregateCall:
		function, err = f.renderCall(c, true)
	default:
		return "", fmt.Errorf("tabula: Over requires a window function or ordinary aggregate")
	}
	if err != nil {
		return "", err
	}
	if n.spec.Name != "" {
		if !typedIdent(n.spec.Name) {
			return "", fmt.Errorf("tabula: invalid window name")
		}
		return function + " OVER " + typedQuote(n.spec.Name), nil
	}
	spec, err := n.spec.render(c)
	return function + " OVER (" + spec + ")", err
}
