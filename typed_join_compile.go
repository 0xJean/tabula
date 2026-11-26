package tabula

import (
	"fmt"
	"slices"
	"strings"
)

func (q ReadSelect) checkGrouping(c *typedCompiler) ([]scalarOperand, bool, error) {
	groups := make([]scalarOperand, len(q.group))
	for i, group := range q.group {
		if isNilBound(group) {
			return nil, false, fmt.Errorf("tabula: GROUP BY requires a declared expression")
		}
		groups[i] = group.groupOperand()
	}
	if err := checkGroupKeys(groups, c.sources); err != nil {
		return nil, false, err
	}
	aggregate, err := groupingExpressions(q.project, q.having, q.order, groups)
	return groups, aggregate, err
}

func (q ReadSelect) render(c *typedCompiler, owner *registration, indexes []int, sources []rowSource, take *typedInput) (string, StatementShape, error) {
	groups, aggregate, err := q.checkGrouping(c)
	if err != nil {
		return "", StatementShape{}, err
	}
	outer := make(map[*occurrence]bool)
	for i, join := range q.joins {
		if join.kind == "LEFT JOIN" {
			outer[sources[i+1].rel] = true
		}
	}
	if q.lock.declared && (aggregate || q.distinct || len(q.distinctOn) > 0 || outer[q.lock.target]) {
		return "", StatementShape{}, fmt.Errorf("tabula: row locks require ungrouped rows on a preserved occurrence")
	}
	if err := q.checkDistinct(groups, aggregate, sources); err != nil {
		return "", StatementShape{}, err
	}
	for _, p := range q.project {
		if err := checkAggregateNulls(p.part, outer); err != nil {
			return "", StatementShape{}, err
		}
	}
	for _, having := range q.having {
		if err := checkAggregateNulls(having.part, outer); err != nil {
			return "", StatementShape{}, err
		}
	}
	for _, order := range q.order {
		if err := checkAggregateNulls(order.orderValue().part, outer); err != nil {
			return "", StatementShape{}, err
		}
	}
	prefix, err := q.distinctSQL(c)
	if err != nil {
		return "", StatementShape{}, err
	}
	selects, result, err := c.projectFields(owner, q.project, indexes)
	if err != nil {
		return "", StatementShape{}, err
	}
	if len(outer) > 0 {
		for i, p := range q.project {
			if nullExtended(p.part, outer) {
				if !owner.def.Columns[p.index].Nullable {
					return "", StatementShape{}, fmt.Errorf("tabula: outer join requires a nullable output for %q", result[i].Name)
				}
				result[i].Nullable = true
			}
		}
	}
	from, where, err := q.fromSQL(c, sources)
	if err != nil {
		return "", StatementShape{}, err
	}
	query := "SELECT " + prefix + strings.Join(selects, ", ") + " FROM " + from + whereSQL(where)
	if len(groups) > 0 {
		terms := make([]string, len(groups))
		for i, f := range groups {
			terms[i], err = f.part.render(c)
			if err != nil {
				return "", StatementShape{}, err
			}
		}
		query += " GROUP BY " + strings.Join(terms, ", ")
	}
	if len(q.having) > 0 {
		terms := make([]string, len(q.having))
		for i, p := range q.having {
			terms[i], err = renderRowPredicate(c, p)
			if err != nil {
				return "", StatementShape{}, err
			}
		}
		query += " HAVING " + strings.Join(terms, " AND ")
	}
	if len(q.order) > 0 {
		order, err := (&rowOrdering{terms: q.order}).orderSQL(c)
		if err != nil {
			return "", StatementShape{}, err
		}
		query += order
	}
	limit, err := renderSingleLimit(c, take)
	if err != nil {
		return "", StatementShape{}, err
	}
	query += limit
	meta, err := c.readRequirements()
	if err != nil {
		return "", StatementShape{}, err
	}
	meta.Result = result
	if q.lock.declared {
		lock, err := q.lock.clause(c, &meta)
		if err != nil {
			return "", StatementShape{}, err
		}
		query += lock
	}
	return query, meta, nil
}

func (q ReadSelect) fromSQL(c *typedCompiler, sources []rowSource) (string, []string, error) {
	var from strings.Builder
	from.WriteString(sources[0].fromSQL())
	visible := map[*occurrence]bool{sources[0].rel: true}
	whereSources := []rowSource{sources[0]}
	for i, join := range q.joins {
		right := sources[i+1]
		visible[right.rel] = true
		from.WriteString(" " + join.kind + " " + right.fromSQL())
		if join.kind == "CROSS JOIN" {
			whereSources = append(whereSources, right)
			continue
		}
		err := checkRowExpr(join.on.part, func(node expressionPart) error {
			traits, _ := expressionStructure(node)
			if traits.aggregate {
				return fmt.Errorf("tabula: aggregate in JOIN condition")
			}
			switch n := node.(type) {
			case *fieldRef:
				if !visible[n.source] {
					return fmt.Errorf("tabula: ON refers to an occurrence outside this join")
				}
			case rowAggregate:
				return fmt.Errorf("tabula: aggregate in JOIN condition")
			}
			return nil
		})
		if err != nil {
			return "", nil, err
		}
		on, err := renderRowPredicate(c, join.on)
		if err != nil {
			return "", nil, err
		}
		terms, err := right.where(c, nil)
		if err != nil {
			return "", nil, err
		}
		from.WriteString(" ON " + on)
		for _, term := range terms {
			from.WriteString(" AND " + term)
		}
	}
	var where []string
	for _, src := range whereSources {
		terms, err := src.where(c, nil)
		if err != nil {
			return "", nil, err
		}
		where = append(where, terms...)
	}
	for _, p := range q.where {
		if err := checkRowExpr(p.part, nil); err != nil {
			return "", nil, err
		}
		term, err := renderRowPredicate(c, p)
		if err != nil {
			return "", nil, err
		}
		where = append(where, term)
	}
	return from.String(), where, nil
}

// The declared expression already tracks base-column and input nullability.
// Only strict operations propagate an additional NULL from a left join.
func nullExtended(part expressionPart, outer map[*occurrence]bool) bool {
	propagates := func(p Pred) bool { return nullExtended(p.part, outer) }
	switch n := part.(type) {
	case describedExpression:
		return n.nullableAfterJoin(outer)
	case *fieldRef:
		return outer[n.source]
	case comparison:
		return n.op != "IS DISTINCT FROM" && n.op != "IS NOT DISTINCT FROM" &&
			(nullExtended(n.left, outer) || nullExtended(n.right, outer))
	case rowOr:
		return slices.ContainsFunc(n, propagates)
	case conjunction:
		return slices.ContainsFunc(n, propagates)
	case rowNot:
		return propagates(n.term)
	case membership:
		return nullExtended(n.value, outer) || slices.ContainsFunc(n.members, func(p expressionPart) bool { return nullExtended(p, outer) })
	default:
		// Inputs, IS NULL and aggregate result types carry their own contracts.
		return false
	}
}
