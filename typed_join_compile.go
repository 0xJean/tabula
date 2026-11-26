package tabula

import (
	"fmt"
	"slices"
	"strings"
)

type occurrenceColumn struct {
	source *occurrence
	name   string
}

func (q ReadSelect) checkGrouping(c *typedCompiler) ([]*fieldRef, bool, error) {
	grouped := make(map[occurrenceColumn]bool)
	fields := make([]*fieldRef, 0, len(q.group))
	for _, group := range q.group {
		if isNilBound(group) || group.groupField() == nil {
			return nil, false, fmt.Errorf("tabula: GROUP BY requires a physical field")
		}
		f := group.groupField()
		key := occurrenceColumn{f.source, f.name}
		if _, ok := c.sources[f.source]; !ok || grouped[key] {
			return nil, false, fmt.Errorf("tabula: duplicate or out-of-scope GROUP BY field %q", f.name)
		}
		grouped[key] = true
		fields = append(fields, f)
	}
	aggregate := len(fields) != 0 || len(q.having) != 0
	var free []*fieldRef
	visit := func(node expressionPart) error {
		switch n := node.(type) {
		case rowAggregate:
			aggregate = true
		case *fieldRef:
			free = append(free, n)
		}
		return nil
	}
	for _, p := range q.project {
		if err := checkRowExpr(p.part, visit); err != nil {
			return nil, false, err
		}
	}
	for _, h := range q.having {
		if err := checkRowExpr(h.part, visit); err != nil {
			return nil, false, err
		}
	}
	for _, term := range q.order {
		if term.field == nil {
			return nil, false, fmt.Errorf("tabula: ORDER BY requires a physical field")
		}
		free = append(free, term.field)
	}
	for _, f := range free {
		if aggregate && !grouped[occurrenceColumn{f.source, f.name}] {
			return nil, false, fmt.Errorf("tabula: ungrouped field %q on occurrence %q", f.name, f.source.alias)
		}
	}
	return fields, aggregate, nil
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
	if q.lock.declared && (aggregate || outer[q.lock.target]) {
		return "", StatementShape{}, fmt.Errorf("tabula: row locks require ungrouped rows on a preserved occurrence")
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
	query := "SELECT " + strings.Join(selects, ", ") + " FROM " + from + whereSQL(where)
	if len(groups) > 0 {
		terms := make([]string, len(groups))
		for i, f := range groups {
			terms[i], err = f.render(c)
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
