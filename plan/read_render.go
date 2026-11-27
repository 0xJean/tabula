package plan

import (
	"github.com/0xJean/tabula"

	"fmt"
	"slices"
	"strings"
)

func (p *readListPlan) render(c *typedCompiler, owner *outputLayout, indexes []int, q ReadSelect, sources []rowSource, form string) (string, tabula.StatementShape, error) {
	c.extendedForm = true
	if form == "list.count" {
		return renderReadCount(c, owner, indexes, q, sources)
	}
	data := p.data[form]
	grouped := len(q.group) > 0 || readHasAggregate(q)
	unordered := q
	unordered.order = nil
	orderAggregates := len(q.group) == 0 && readHasAggregate(q) && !readHasAggregate(unordered)
	wrapped := len(q.distinctOn) > 0 && data.order != nil || data.page == AfterPage && (grouped || q.distinct || q.hasWindows()) || orderAggregates && data.order != nil
	if data.order != nil && !wrapped {
		q.order = data.order.terms
	}
	if wrapped && len(q.distinctOn) == 0 && !orderAggregates {
		q.order = nil
	}
	if data.page == AfterPage && !wrapped {
		q = q.Where(Pred{part: readSeek{data.order, nil}, kind: Bool()})
	}
	query, meta, err := q.render(c, owner, indexes, sources, nil)
	if err != nil {
		return "", tabula.StatementShape{}, err
	}
	if wrapped {
		if q.hasRowLocks() {
			return "", tabula.StatementShape{}, fmt.Errorf("tabula: a wrapped result cannot be row locked")
		}
		cols := make([]string, len(q.project))
		for i, project := range q.project {
			cols[i] = `"result".` + typedQuote(project.field.Name)
		}
		query = "SELECT " + strings.Join(cols, ", ") + " FROM (" + query + `) AS "result"`
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
	}
	page, err := p.page.pageSQL(c, data)
	if err != nil {
		return "", tabula.StatementShape{}, err
	}
	// Row locks must follow LIMIT/OFFSET. q.render already emitted the lock.
	if q.hasRowLocks() {
		lock, err := q.rowLockClauses(c, &tabula.StatementShape{})
		if err != nil {
			return "", tabula.StatementShape{}, err
		}
		query = strings.TrimSuffix(query, lock) + page + lock
	} else {
		query += page
	}
	if len(q.distinctOn) > 0 {
		meta.Keys, meta.Comparisons, err = readWinnerProof(q, sources)
		if err != nil {
			return "", tabula.StatementShape{}, err
		}
	}
	if data.page == FirstPage || data.page == AfterPage {
		keys, comparisons, err := readResultProof(q, sources, data.order.terms)
		if err != nil {
			return "", tabula.StatementShape{}, err
		}
		meta.Keys = appendKeyRequirements(meta.Keys, keys...)
		meta.Comparisons = appendComparisonRequirements(meta.Comparisons, comparisons...)
	}
	return query, meta, nil
}

func projectedName(part expressionPart, projection []Projection) (string, error) {
	for _, p := range projection {
		if sameExpression(part, p.part) {
			return `"result".` + typedQuote(p.field.Name), nil
		}
	}
	return "", fmt.Errorf("tabula: result order must use a projected expression")
}

func projectedOrder(o *rowOrdering, projection []Projection) (string, error) {
	terms := make([]string, len(o.terms))
	for i, term := range o.terms {
		name, err := projectedName(term.orderValue().part, projection)
		if err != nil {
			return "", err
		}
		direction, nulls := " ASC", " NULLS LAST"
		if term.desc {
			direction = " DESC"
		}
		if term.nullsFirst() {
			nulls = " NULLS FIRST"
		}
		terms[i] = name + direction + nulls
	}
	return " ORDER BY " + strings.Join(terms, ", "), nil
}

type readSeek struct {
	order      *rowOrdering
	projection []Projection
}

func (s readSeek) expressionTraits() expressionTraits {
	parts := make([]expressionPart, 0, len(s.order.terms)*2)
	for i, term := range s.order.terms {
		parts = append(parts, term.orderValue().part, s.order.cursors[i])
	}
	return expressionTraits{identity: "read-seek", children: parts}
}
func (readSeek) nullableAfterJoin(map[*occurrence]bool) bool { return false }
func (s readSeek) render(c *typedCompiler) (string, error) {
	var disjunction, prefix []string
	for i, term := range s.order.terms {
		var value string
		var err error
		if s.projection != nil {
			value, err = projectedName(term.orderValue().part, s.projection)
		} else {
			value, err = term.orderValue().part.render(c)
		}
		if err != nil {
			return "", err
		}
		slot, err := s.order.cursors[i].render(c)
		if err != nil {
			return "", err
		}
		operation := "gt"
		op := " OPERATOR(pg_catalog.>) "
		if term.desc {
			op = " OPERATOR(pg_catalog.<) "
			operation = "lt"
		}
		if err := requireReadComparison(c, term.orderValue().kind, operation); err != nil {
			return "", err
		}
		nulls := "(" + slot + " IS NOT NULL AND " + value + " IS NULL)"
		if term.nullsFirst() {
			nulls = "(" + slot + " IS NULL AND " + value + " IS NOT NULL)"
		}
		greater := "(" + value + op + slot + " OR " + nulls + ")"
		terms := append(slices.Clone(prefix), greater)
		disjunction = append(disjunction, "("+strings.Join(terms, " AND ")+")")
		if i < len(s.order.terms)-1 {
			if err := requireReadComparison(c, term.orderValue().kind, "eq"); err != nil {
				return "", err
			}
			prefix = append(prefix, "("+value+" OPERATOR(pg_catalog.=) "+slot+" OR ("+value+" IS NULL AND "+slot+" IS NULL))")
		}
	}
	return "(" + strings.Join(disjunction, " OR ") + ")", nil
}

func readHasAggregate(q ReadSelect) bool {
	var has func(expressionPart) bool
	has = func(p expressionPart) bool {
		t, ok := expressionStructure(p)
		return ok && (t.aggregate || slices.ContainsFunc(t.children, has))
	}
	for _, p := range q.project {
		if has(p.part) {
			return true
		}
	}
	for _, p := range q.having {
		if has(p.part) {
			return true
		}
	}
	for _, term := range q.order {
		if has(term.orderValue().part) {
			return true
		}
	}
	return len(q.having) > 0
}

func renderReadCount(c *typedCompiler, owner *outputLayout, indexes []int, q ReadSelect, sources []rowSource) (string, tabula.StatementShape, error) {
	c.requireNative("pg_catalog.count()")
	scalar := len(q.group) == 0 && readHasAggregate(q)
	q.order, q.lock = nil, RowLock{}
	grouped := len(q.group) > 0 || scalar
	if scalar {
		q.distinct, q.distinctOn = false, nil
	}
	wrap := grouped || q.distinct || len(q.distinctOn) > 0
	if !q.distinct {
		var values []scalarOperand
		if len(q.distinctOn) > 0 {
			values, q.distinct, q.distinctOn = q.distinctOn, true, nil
		} else {
			values = []scalarOperand{CountAll().scalarOperand()}
		}
		entry := new(registration)
		owner = &outputLayout{
			entry:     entry,
			secrets:   make([]bool, len(values)),
			nullables: slices.Repeat([]bool{true}, len(values)),
		}
		indexes = nil
		q.project = make([]Projection, len(values))
		for i, value := range values {
			name := fmt.Sprintf("value_%d", i+1)
			q.project[i] = Projection{target: entry, index: i, field: tabula.ShapeField{Name: name, Type: value.kind.Qualified(), Nullable: value.nullable}, part: value.part, compatible: true, kind: value.kind}
		}
		if len(values) == 1 && !q.distinct {
			q.project[0].field.Name = "count"
		}
	}
	inner, meta, err := q.render(c, owner, indexes, sources, nil)
	if err != nil {
		return "", tabula.StatementShape{}, err
	}
	if wrap {
		inner = `SELECT pg_catalog.count(*) AS "count" FROM (` + inner + `) AS "counted"`
	}
	meta.Result = []tabula.ShapeField{{Name: "count", Type: typeID(Int8()).Qualified()}}
	return inner, meta, nil
}
