package tabula

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// RowsSpec declares the finite alternatives for a single-table read.
// GroupBy lists the fields or GroupExpr terms that determine each group.
// Named orders share one exact count. Each order must preserve the result
// cardinality, including the one-row result of an ungrouped aggregate on
// empty input.
type RowsSpec struct {
	RowFilter
	GroupBy []RowColumn
	Orders  []RowOrder
	Pages   RowPages
	// Lock applies to every data page and bounded single-row form. It
	// requires an ungrouped, non-aggregate projection and excludes summaries.
	Lock RowLock
	// WithExists registers a boolean existence check for each filter and
	// scope choice of an unlocked, non-aggregate plan.
	WithExists bool
}

// RowsPlan owns the registered read shapes for one result descriptor.
type RowsPlan[T, S any] struct {
	owner      *descriptor[T, S]
	indexes    []int
	single     singleRead
	family     *rowFamily
	page       rowPagePlan
	lock       RowLock
	withExists bool
}

// DefineRows registers a named single-table read. It panics on declaration errors.
func DefineRows[T, R, S any](out *View[T, S], name string, src RowSource[R, S], spec RowsSpec, project ...Projection) *RowsPlan[T, S] {
	p, err := TryDefineRows(out, name, src, spec, project...)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineRows registers the complete read plan or returns an error without
// publishing any shape. The projection covers the view in descriptor order.
func TryDefineRows[T, R, S any](out *View[T, S], name string, src RowSource[R, S], spec RowsSpec, project ...Projection) (*RowsPlan[T, S], error) {
	return TryDefineRowsWithForms(out, name, src, spec, ReadForms{}, project...)
}

// DefineRowsWithForms registers a single-table read with the declared bounded
// single-row forms. It panics on a declaration error.
func DefineRowsWithForms[T, R, S any](out *View[T, S], name string, src RowSource[R, S], spec RowsSpec, forms ReadForms, project ...Projection) *RowsPlan[T, S] {
	p, err := TryDefineRowsWithForms(out, name, src, spec, forms, project...)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineRowsWithForms registers the data, summary and requested bounded
// forms together. A rejected declaration publishes no shapes.
func TryDefineRowsWithForms[T, R, S any](out *View[T, S], name string, src RowSource[R, S], spec RowsSpec, forms ReadForms, project ...Projection) (*RowsPlan[T, S], error) {
	if out == nil {
		return nil, fmt.Errorf("tabula: a declared output and row source are required")
	}
	return compileRowsPlan(&out.descriptor, nil, name, src, spec, forms, project)
}

func compileRowsPlan[T, R, S any](owner *descriptor[T, S], indexes []int, name string, src RowSource[R, S],
	spec RowsSpec, policy ReadForms, project []Projection,
) (*RowsPlan[T, S], error) {
	if owner == nil || src.owner == nil {
		return nil, fmt.Errorf("tabula: a declared output and row source are required")
	}
	aggregate, err := checkRowGrouping(src.rowSource, project, spec)
	if err != nil {
		return nil, err
	}
	locked := spec.Lock.declared
	if locked && aggregate {
		return nil, fmt.Errorf("tabula: a row lock requires a non-aggregate, ungrouped projection")
	}
	if spec.WithExists && (aggregate || locked) {
		return nil, fmt.Errorf("tabula: WithExists requires an unlocked, non-aggregate plan")
	}
	single, err := newSingleRead(policy, len(spec.Orders) > 0, !locked && spec.Pages.includesUnpaged())
	if err != nil {
		return nil, err
	}
	p := &RowsPlan[T, S]{owner: owner, indexes: slices.Clone(indexes), single: single, lock: spec.Lock, withExists: spec.WithExists,
		page: rowPagePlan{pages: spec.Pages, orders: make(map[string]*rowOrdering)}}
	for _, order := range spec.Orders {
		if p.page.orders[order.Name] != nil {
			return nil, fmt.Errorf("tabula: duplicate row order %q", order.Name)
		}
		o, err := newRowOrder(order, src.rowSource, spec.GroupBy, aggregate, spec.Pages.Keyset, project)
		if err != nil {
			return nil, err
		}
		p.page.orders[o.name] = o
	}
	forms, err := p.page.forms(single)
	if err != nil {
		return nil, err
	}
	var names []string
	if !locked {
		names = append(names, "count")
	}
	if spec.WithExists {
		names = append(names, "exists")
	}
	names = append(names, slices.Sorted(maps.Keys(forms))...)
	expressionForms := spec.hasExpressionForms(project)
	p.family, err = compileRowFamily(owner, name, []rowSource{src.rowSource}, spec.RowFilter, names,
		func(c *typedCompiler, form string, predicates []Pred) (string, StatementShape, error) {
			c.extendedForm = expressionForms
			if form == "exists" {
				return renderRowExists(c, src.rowSource, predicates)
			}
			return p.render(c, src.rowSource, project, spec.GroupBy, aggregate, predicates, forms[form], form == "count")
		})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Count drops presentation expressions. Classify the declaration once so a
// separately bound summary still exports the aggregate it executes, even if
// the only new expression belongs to a projection or an optional filter.
func (s RowsSpec) hasExpressionForms(project []Projection) bool {
	if slices.ContainsFunc(s.GroupBy, func(g RowColumn) bool { return g.ref == nil }) ||
		slices.ContainsFunc(project, func(p Projection) bool { return hasNewScalarOrAggregate(p.part) }) ||
		hasNewScalarOrAggregate(s.Where.part) ||
		slices.ContainsFunc(s.Optional, func(p RowPredicate) bool { return hasNewScalarOrAggregate(p.When.part) }) {
		return true
	}
	for _, order := range s.Orders {
		for _, term := range order.Terms {
			if term.field == nil || term.nulls != defaultNulls {
				return true
			}
		}
	}
	return false
}

func hasNewScalarOrAggregate(p expressionPart) bool {
	switch p.(type) {
	case scalarNode, aggregateCall:
		return true
	}
	traits, ok := expressionStructure(p)
	return ok && slices.ContainsFunc(traits.children, hasNewScalarOrAggregate)
}

func checkRowGrouping(src rowSource, project []Projection, spec RowsSpec) (bool, error) {
	values := rowGroupOperands(spec.GroupBy)
	if err := checkGroupKeys(values, map[*occurrence]bool{src.rel: true}); err != nil {
		return false, err
	}
	if !spec.Lock.declared {
		if err := checkRowCountStability(spec); err != nil {
			return false, err
		}
	}
	aggregate, err := groupingExpressions(project, nil, nil, values)
	if err != nil {
		return false, err
	}
	for i, order := range spec.Orders {
		grouped, err := groupingExpressions(project, nil, order.Terms, values)
		if err != nil {
			return false, fmt.Errorf("tabula: ungrouped order or projection in %q: %w", order.Name, err)
		}
		// Count has no order choice. Every presentation of this result
		// must agree on whether an empty input yields a scalar group.
		if i > 0 && aggregate != grouped {
			return false, fmt.Errorf("tabula: order %q changes the result cardinality", order.Name)
		}
		aggregate = grouped
	}
	return aggregate, nil
}

func (p *RowsPlan[T, S]) render(c *typedCompiler, src rowSource, project []Projection, groups []RowColumn,
	aggregate bool, predicates []Pred, data rowDataForm, count bool,
) (string, StatementShape, error) {
	var selectSQL string
	var fields []ShapeField
	if count {
		c.legacyAggregates = append(c.legacyAggregates, aggregateCall{fn: "count", star: true})
		selectSQL = `pg_catalog.count(*) AS "count"`
		fields = []ShapeField{{Name: "count", Type: Int8().id.qualified()}}
	} else {
		columns, result, err := c.projectFields(p.owner.entry, project, p.indexes)
		if err != nil {
			return "", StatementShape{}, err
		}
		selectSQL, fields = strings.Join(columns, ", "), result
	}
	terms, err := src.where(c, predicates)
	if err != nil {
		return "", StatementShape{}, err
	}
	order := data.order
	prove := (data.page == FirstPage || data.page == AfterPage) && order.needsResultProof(groups)
	resultSeek := data.page == AfterPage && aggregate && complexRowOrder(data.order, groups)
	if data.page == AfterPage && !resultSeek {
		after, err := order.afterSQL(c, prove)
		if err != nil {
			return "", StatementShape{}, err
		}
		terms = append(terms, after)
	}
	from := " FROM " + src.fromSQL() + whereSQL(terms)
	if len(groups) > 0 {
		parts := make([]string, len(groups))
		for i, g := range groups {
			part, err := g.groupOperand().part.render(c)
			if err != nil {
				return "", StatementShape{}, err
			}
			parts[i] = part
		}
		from += " GROUP BY " + strings.Join(parts, ", ")
	}
	query := "SELECT " + selectSQL + from
	if count && aggregate {
		// A scalar aggregate yields one row even for an empty input. A
		// grouped aggregate yields one row per group, including NULL groups.
		query = "SELECT " + selectSQL + " FROM (SELECT pg_catalog.count(*)" + from + `) AS "groups"`
	}
	if resultSeek {
		resultOrder := projectedRowOrder(data.order, project, "rows")
		after, err := resultOrder.afterSQL(c, true)
		if err != nil {
			return "", StatementShape{}, err
		}
		selected := make([]string, len(project))
		for i, p := range project {
			selected[i] = typedQuote("rows") + "." + typedQuote(p.field.Name)
		}
		query = "SELECT " + strings.Join(selected, ", ") + " FROM (" + query + ") AS \"rows\" WHERE " + after
		data.order = resultOrder
	}
	if !count {
		order, err := data.order.orderSQL(c)
		if err != nil {
			return "", StatementShape{}, err
		}
		page, err := p.page.pageSQL(c, data)
		if err != nil {
			return "", StatementShape{}, err
		}
		query += order + page
	}
	meta, err := c.readRequirements()
	if err != nil {
		return "", StatementShape{}, err
	}
	meta.Result = fields
	if prove {
		selection := ReadSelect{project: project, group: rowGroupFields(groups)}
		meta.Keys, meta.Comparisons, err = readResultProof(selection, []rowSource{src}, order.terms)
		if err != nil {
			return "", StatementShape{}, err
		}
	}
	if p.lock.declared {
		clause, err := p.lock.clause(c, &meta)
		if err != nil {
			return "", StatementShape{}, err
		}
		query += clause
	}
	return query, meta, nil
}

func renderRowExists(c *typedCompiler, src rowSource, predicates []Pred) (string, StatementShape, error) {
	terms, err := src.where(c, predicates)
	if err != nil {
		return "", StatementShape{}, err
	}
	meta, err := c.readRequirements()
	if err != nil {
		return "", StatementShape{}, err
	}
	meta.Result = []ShapeField{{Name: "exists", Type: Bool().id.qualified()}}
	return `SELECT EXISTS (SELECT 1 FROM ` + src.fromSQL() + whereSQL(terms) + `) AS "exists"`, meta, nil
}

// Cursor validates a boundary against the specific plan, order, field types
// and arity. Supply values in order, taken from the last row of the prior page.
func (p *RowsPlan[T, S]) Cursor(order string, values ...CursorValue) (RowCursor, error) {
	if p == nil || !p.page.pages.Keyset {
		return RowCursor{}, fmt.Errorf("tabula: unknown keyset order")
	}
	return cursorForOrder(p.page.orders[order], values)
}

// CursorFrom extracts the projected ordering values from a decoded row, using
// their declared input codecs. The boundary belongs to this plan and order.
func (p *RowsPlan[T, S]) CursorFrom(order string, row T) (RowCursor, error) {
	if p == nil || !p.page.pages.Keyset {
		return RowCursor{}, fmt.Errorf("tabula: unknown keyset order")
	}
	return cursorFromRow(p.owner, p.page.orders[order], row)
}
