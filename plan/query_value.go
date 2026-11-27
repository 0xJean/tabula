package plan

import "fmt"

// ValueSelect is a one-column query with an implicit exported identity.
// It is composed without creating a View or registering an executable plan.
type ValueSelect[V any] struct {
	query ReadSelect
	value Expr[V]
}

// SelectValue starts a scalar-result query.
func SelectValue[V any](value Expr[V]) ValueSelect[V] {
	return ValueSelect[V]{Select(Export("value", value).Project()), value}
}

// From declares the first query occurrence.
func (q ValueSelect[V]) From(source ReadSource) ValueSelect[V] {
	q.query = q.query.From(source)
	return q
}

// Where filters input rows.
func (q ValueSelect[V]) Where(terms ...Pred) ValueSelect[V] {
	q.query = q.query.Where(terms...)
	return q
}

// GroupBy declares grouping expressions.
func (q ValueSelect[V]) GroupBy(fields ...GroupField) ValueSelect[V] {
	q.query = q.query.GroupBy(fields...)
	return q
}

// Having filters aggregate groups.
func (q ValueSelect[V]) Having(terms ...Pred) ValueSelect[V] {
	q.query = q.query.Having(terms...)
	return q
}

// OrderBy declares result ordering.
func (q ValueSelect[V]) OrderBy(terms ...RowTerm) ValueSelect[V] {
	q.query = q.query.OrderBy(terms...)
	return q
}

// InnerJoin declares an explicit matching occurrence.
func (q ValueSelect[V]) InnerJoin(source ReadSource, on Pred) ValueSelect[V] {
	q.query = q.query.InnerJoin(source, on)
	return q
}

// LeftJoin preserves unmatched left rows.
func (q ValueSelect[V]) LeftJoin(source ReadSource, on Pred) ValueSelect[V] {
	q.query = q.query.LeftJoin(source, on)
	return q
}

// CrossJoin declares a product.
func (q ValueSelect[V]) CrossJoin(source ReadSource) ValueSelect[V] {
	q.query = q.query.CrossJoin(source)
	return q
}

// InnerLateral joins a query using explicitly declared preceding references.
func (q ValueSelect[V]) InnerLateral(source QuerySource, on Pred) ValueSelect[V] {
	q.query = q.query.InnerLateral(source, on)
	return q
}

// LeftLateral preserves left rows when the lateral result is empty.
func (q ValueSelect[V]) LeftLateral(source QuerySource, on Pred) ValueSelect[V] {
	q.query = q.query.LeftLateral(source, on)
	return q
}

// CrossLateral declares an explicit lateral product.
func (q ValueSelect[V]) CrossLateral(source QuerySource) ValueSelect[V] {
	q.query = q.query.CrossLateral(source)
	return q
}

// With attaches common-table declarations.
func (q ValueSelect[V]) With(tables ...CommonTable) ValueSelect[V] {
	q.query = q.query.With(tables...)
	return q
}

// Take applies a nonnegative fixed bound through a private parameter.
func (q ValueSelect[V]) Take(n int64) ValueSelect[V] { q.query = q.query.Take(n); return q }

// Limit applies a caller-bound limit with a finite ceiling checked before I/O.
func (q ValueSelect[V]) Limit(input Slot[int64], ceiling int64) ValueSelect[V] {
	q.query = q.query.Limit(input, ceiling)
	return q
}

// Distinct removes duplicate values.
func (q ValueSelect[V]) Distinct() ValueSelect[V] { q.query = q.query.Distinct(); return q }

type queryLimit struct {
	input   *typedInput
	ceiling int64
}

// Take bounds rows with an internal parameter. Negative bounds fail definition.
func (q ReadSelect) Take(n int64) ReadSelect {
	input := &typedInput{name: "@take", kind: typeID(Int8()), fixed: new(n)}
	q.limit = &queryLimit{input, n}
	return q
}

// Limit bounds rows with an int8 input and a nonnegative declared maximum.
// NULL, negative and excessive runtime values fail before execution.
func (q ReadSelect) Limit(input Slot[int64], ceiling int64) ReadSelect {
	q.limit = &queryLimit{input.input, ceiling}
	return q
}
func (q *queryLimit) render(c *typedCompiler) (string, error) {
	if q == nil {
		return "", nil
	}
	if q.input == nil || q.ceiling < 0 || !sameSQLType(q.input.kind, typeID(Int8())) {
		return "", fmt.Errorf("tabula: LIMIT requires an int8 input and a nonnegative ceiling")
	}
	clause, err := renderSingleLimit(c, q.input)
	if err != nil {
		return "", err
	}
	input := q.input
	if alias := c.inputAliases[input]; alias != nil {
		input = alias
	}
	if c.params.limits == nil {
		c.params.limits = make(map[*typedInput]int64)
	}
	if old, ok := c.params.limits[input]; !ok || q.ceiling < old {
		c.params.limits[input] = q.ceiling
	}
	return clause, nil
}

// Outer moves all references in value exactly one lexical query level outward.
// Nest it explicitly for a grandparent. Aggregate ownership is never inferred.
func Outer[V any](value Expr[V]) Expr[V] {
	return Expr[V]{queryOuter{value.scalarOperand()}, value.kind, value.nullable}
}

type queryOuter struct{ value scalarOperand }

func (o queryOuter) expressionTraits() expressionTraits {
	return expressionTraits{identity: "outer", children: []expressionPart{o.value.part}}
}
func (o queryOuter) nullableAfterJoin(outer map[*occurrence]bool) bool {
	return o.value.nullableWith(outer)
}
func (o queryOuter) render(c *typedCompiler) (string, error) {
	if c.block == nil {
		return "", fmt.Errorf("tabula: Outer requires an enclosing query")
	}
	if hasAggregate(o.value.part) {
		return "", fmt.Errorf("tabula: export the outer aggregate before referencing it")
	}
	parent, err := c.block.ancestor(1)
	if err != nil {
		return "", err
	}
	previous := c.block
	c.block = parent
	defer func() { c.block = previous }()
	if o.value.part == nil {
		return "", fmt.Errorf("tabula: undeclared outer expression")
	}
	return o.value.part.render(c)
}
