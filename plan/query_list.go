package plan

import (
	"github.com/0xJean/tabula"

	"fmt"
	"slices"
	"strings"
)

func (p *readListPlan) renderQuery(ctx *statementContext, q ReadSelect, form string) (string, tabula.StatementShape, error) {
	result, err := q.renderStatement(ctx)
	if err != nil {
		return "", tabula.StatementShape{}, err
	}
	if tabula.ShapeEffects(result.effects.Bits)&tabula.EffectWrite != 0 {
		return "", tabula.StatementShape{}, fmt.Errorf("tabula: a read list cannot execute write effects")
	}
	if tabula.ShapeEffects(result.effects.Bits)&tabula.EffectRowLock != 0 {
		return "", tabula.StatementShape{}, fmt.Errorf("tabula: composed list results cannot carry row locks")
	}
	c := ctx.compiler
	if form == "list.count" {
		if err := queryCountStability(q, make(map[*commonTableNode]bool)); err != nil {
			return "", tabula.StatementShape{}, err
		}
		c.requireNative("pg_catalog.count()")
		result.shape.Result = []tabula.ShapeField{{Name: "count", Type: typeID(Int8()).Qualified()}}
		result.shape.ResultLineage = nil
		return `SELECT pg_catalog.count(*) AS "count" FROM (` + result.sql + `) AS "counted"`, result.shape, nil
	}
	data := p.data[form]
	var names []string
	for _, f := range result.fields {
		names = append(names, `"result".`+typedQuote(f.name))
	}
	query := "SELECT " + strings.Join(names, ", ") + " FROM (" + result.sql + `) AS "result"`
	if data.order != nil {
		for i, term := range data.order.terms {
			for _, f := range result.fields {
				if sameExpression(term.orderValue().part, f.value.part) && len(data.order.nullable) > i {
					data.order.nullable[i] = data.order.nullable[i] || f.value.nullable
				}
			}
		}
	}
	if data.page == FirstPage || data.page == AfterPage {
		proof, err := queryFinalResultProof(result, data.order.terms)
		if err != nil {
			return "", tabula.StatementShape{}, err
		}
		result.shape.Keys = appendKeyRequirements(result.shape.Keys, proof.keys...)
		result.shape.Comparisons = appendComparisonRequirements(result.shape.Comparisons, proof.comparisons...)
	}
	if data.page == AfterPage {
		after, err := (readSeek{data.order, q.project}).render(c)
		if err != nil {
			return "", tabula.StatementShape{}, err
		}
		query += " WHERE " + after
	}
	if data.order != nil {
		order, err := projectedOrder(data.order, q.project)
		if err != nil {
			return "", tabula.StatementShape{}, err
		}
		query += order
	}
	page, err := p.page.pageSQL(c, data)
	return query + page, result.shape, err
}

func queryFinalResultProof(result statementResult, order []RowTerm) (queryValueProof, error) {
	proof := queryValueProof{valid: true, exact: true}
	var ordered []*queryFieldIdentity
	for _, term := range order {
		id := queryProjectedIdentity(result.fields, term.orderValue().part)
		if id == nil {
			return queryValueProof{}, fmt.Errorf("tabula: result order must use a projected expression")
		}
		ordered = append(ordered, id)
		for _, field := range result.fields {
			if field.id == id {
				if !field.proof.valid || field.stability != expressionImmutable {
					return queryValueProof{}, fmt.Errorf("tabula: result cursor requires a proved repeatable value and representation")
				}
				proof = proof.combine(field.proof)
			}
		}
	}
	for _, key := range result.keys {
		if len(key.fields) != 0 && !slices.ContainsFunc(key.fields, func(id *queryFieldIdentity) bool { return !slices.Contains(ordered, id) }) {
			proof.keys = appendKeyRequirements(proof.keys, key.keys...)
			proof.comparisons = appendComparisonRequirements(proof.comparisons, key.comparisons...)
			return proof, nil
		}
	}
	return queryValueProof{}, fmt.Errorf("tabula: keyset order requires a proved complete result key")
}

func queryCountStability(q ReadSelect, seen map[*commonTableNode]bool) error {
	for _, part := range q.countExpressions() {
		if err := queryCountValue(part, seen); err != nil {
			return err
		}
	}
	if q.set != nil {
		for _, arm := range q.set.arms {
			if q.set.operation != "UNION ALL" {
				for _, field := range arm.queryDeclaredFields() {
					if err := queryCountValue(field.value.part, seen); err != nil {
						return err
					}
				}
			}
			if err := queryCountStability(arm, seen); err != nil {
				return err
			}
		}
	}
	for _, source := range append([]ReadSource{q.from}, queryJoinSources(q.joins)...) {
		if result, ok := source.(QuerySource); ok && result.occurrence != nil {
			switch nested := result.occurrence.node.(type) {
			case ReadSelect:
				if err := queryCountStability(nested, seen); err != nil {
					return err
				}
			case unnestQuery:
				for _, column := range nested.columns {
					if column != nil && !column.ordinal {
						if err := queryCountValue(column.array.part, seen); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	for _, table := range q.tables {
		if table.node == nil || seen[table.node] {
			continue
		}
		seen[table.node] = true
		if nested, ok := table.node.query.(ReadSelect); ok {
			if err := queryCountStability(nested, seen); err != nil {
				return err
			}
		}
		if table.node.step != nil {
			if err := queryCountStability(*table.node.step, seen); err != nil {
				return err
			}
			// A step value can control admission on the next iteration.
			for _, field := range table.node.step.queryDeclaredFields() {
				if err := queryCountValue(field.value.part, seen); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func queryCountValue(part expressionPart, seen map[*commonTableNode]bool) error {
	if expressionHasVolatile(part) {
		return fmt.Errorf("tabula: exact count cardinality cannot depend on a volatile expression")
	}
	switch n := part.(type) {
	case queryExpression:
		if err := queryCountStability(n.query, seen); err != nil {
			return err
		}
		if n.mode != "exists" {
			for _, field := range n.query.queryDeclaredFields() {
				if err := queryCountValue(field.value.part, seen); err != nil {
					return err
				}
			}
		}
	case queryResultRef:
		if field, ok := n.field(); ok && field.value.part != nil {
			return queryCountValue(field.value.part, seen)
		}
		if n.source != nil {
			if source, ok := n.source.node.(unnestQuery); ok {
				for _, column := range source.columns {
					if column != nil && !column.ordinal {
						if err := queryCountValue(column.array.part, seen); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	traits, _ := expressionStructure(part)
	for _, child := range traits.children {
		if err := queryCountValue(child, seen); err != nil {
			return err
		}
	}
	return nil
}
