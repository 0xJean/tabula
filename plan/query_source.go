package plan

import (
	"github.com/0xJean/tabula"

	"fmt"
	"slices"
)

// QuerySource is an occurrence of a typed query result. Its fields retain their
// declaration identities; its alias never creates a physical table or grant.
type QuerySource struct{ occurrence *queryOccurrence }

func (q QuerySource) readSource() (rowSource, error) {
	return rowSource{}, fmt.Errorf("tabula: a query result requires the composed query compiler")
}

// As creates a distinct occurrence of the same result schema.
func (q QuerySource) As(alias string) QuerySource {
	if q.occurrence == nil {
		return QuerySource{}
	}
	return QuerySource{&queryOccurrence{alias: alias, node: q.occurrence.node, fields: cloneQueryFields(q.occurrence.fields)}}
}

// Derived exposes a SELECT as a typed result occurrence. Correlation requires
// an explicit lateral join and Outer references.
func Derived(alias string, query ReadSelect) QuerySource {
	return QuerySource{&queryOccurrence{alias: alias, node: query, fields: query.queryDeclaredFields()}}
}

// Exported identifies one intermediate output without a registered View.
type Exported[V any] struct {
	id     *queryFieldIdentity
	name   string
	value  Expr[V]
	unnest *unnestColumn
}

// Export names an intermediate output and retains its type, codec and origin.
func Export[V any](name string, value Expr[V]) Exported[V] {
	return Exported[V]{id: new(queryFieldIdentity), name: name, value: value}
}

// Project selects this exact exported identity into an intermediate result.
func (f Exported[V]) Project() Projection {
	return Projection{field: tabula.ShapeField{Name: f.name, Type: typeID(f.value.kind).Qualified(), Nullable: f.value.nullable}, part: f.value.part,
		compatible: f.id != nil && typedIdent(f.name), kind: typeID(f.value.kind), id: f.id}
}

// From references this export on a result occurrence that actually supplies it.
func (f Exported[V]) From(source QuerySource) Expr[V] {
	return resultExpression(source, f.id, f.value.kind)
}

// ResultOf references a projected output handle on an intermediate source.
func ResultOf[V any](source QuerySource, field OutputField[V]) Expr[V] {
	return resultExpression(source, field.id, field.kind)
}

func resultExpression[V any](source QuerySource, id *queryFieldIdentity, kind tabula.PGType[V]) Expr[V] {
	ref := queryResultRef{source: source.occurrence, id: id}
	nullable := true
	if source.occurrence != nil {
		for _, f := range source.occurrence.fields {
			if f.id == id {
				nullable = f.value.nullable
			}
		}
	}
	return Expr[V]{part: ref, kind: kind, nullable: nullable}
}

type queryResultRef struct {
	source *queryOccurrence
	id     *queryFieldIdentity
}

func (r queryResultRef) field() (queryResultField, bool) {
	if r.source != nil {
		for _, f := range r.source.fields {
			if f.id == r.id && r.id != nil {
				return f, true
			}
		}
	}
	return queryResultField{}, false
}
func (r queryResultRef) expressionTraits() expressionTraits {
	f, ok := r.field()
	if !ok {
		return expressionTraits{identity: "undeclared result", stability: expressionVolatile}
	}
	return expressionTraits{identity: "query result", stability: f.stability}
}
func (r queryResultRef) nullableAfterJoin(map[*occurrence]bool) bool {
	f, ok := r.field()
	return !ok || f.value.nullable
}
func (r queryResultRef) render(c *typedCompiler) (string, error) {
	if c.block == nil {
		return "", fmt.Errorf("tabula: result field requires a query block")
	}
	f, err := c.block.resolveResult(r.source, r.id, 0)
	if err != nil {
		return "", err
	}
	c.hasSecret = c.hasSecret || f.secret
	return typedQuote(c.block.resultAliases[r.source]) + "." + typedQuote(f.name), nil
}

func (q ReadSelect) queryDeclaredFields() []queryResultField {
	if q.set != nil {
		fields := q.set.arms[0].queryDeclaredFields()
		for _, arm := range q.set.arms[1:] {
			other := arm.queryDeclaredFields()
			for i := range fields {
				if i < len(other) {
					fields[i].value.nullable = fields[i].value.nullable || other[i].value.nullable
					fields[i].stored = fields[i].stored && other[i].stored
					fields[i].stability = max(fields[i].stability, other[i].stability)
				}
			}
		}
		return fields
	}
	fields := make([]queryResultField, len(q.project))
	outer := q.queryOuterPhysical()
	for i, p := range q.project {
		fields[i] = queryResultField{id: p.id, name: p.field.Name, value: scalarOperand{p.part, p.kind, p.field.Nullable || nullExtended(p.part, outer)}, stability: queryStability(p.part), secret: p.secret}
		fields[i].origins, fields[i].secret = queryOrigins(p.part, fields[i].secret)
		if ref, ok := p.part.(queryResultRef); ok {
			if source, found := ref.field(); found {
				fields[i].stored = source.stored
			}
		}
		fields[i].value.nullable = fields[i].value.nullable || q.queryResultNull(p.part)
	}
	return fields
}

func queryStability(part expressionPart) expressionStability {
	t, ok := expressionStructure(part)
	if !ok {
		return expressionVolatile
	}
	result := t.stability
	for _, child := range t.children {
		result = max(result, queryStability(child))
	}
	return result
}
func queryOrigins(part expressionPart, secret bool) ([]physicalColumn, bool) {
	var fields []physicalColumn
	switch n := part.(type) {
	case *fieldRef:
		if n != nil && n.source != nil {
			fields = append(fields, physicalColumn{Table: n.source.table, Name: n.name})
			secret = secret || n.secret
		}
	case *typedInput:
		if n != nil {
			secret = secret || n.secret
		}
	case queryResultRef:
		if f, ok := n.field(); ok {
			fields = slices.Clone(f.origins)
			secret = secret || f.secret
		}
	}
	t, _ := expressionStructure(part)
	for _, child := range t.children {
		origins, s := queryOrigins(child, secret)
		fields = append(fields, origins...)
		secret = secret || s
	}
	return fields, secret
}
