package tabula

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// RowsSpec declares the finite alternatives for a single-table read.
// GroupBy lists the physical fields that determine each aggregate group.
type RowsSpec struct {
	RowFilter
	GroupBy []RowColumn
	Orders  []RowOrder
	Pages   RowPages
	// Lock applies to every data page. A locking plan supports Find only
	// and requires an ungrouped, non-aggregate projection.
	Lock RowLock
	// WithExists registers a boolean existence check for each filter and
	// scope choice of an unlocked, non-aggregate plan.
	WithExists bool
}

// RowsPlan owns the registered read shapes for one output view.
type RowsPlan[T, S any] struct {
	view       *View[T, S]
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
	if out == nil || src.owner == nil {
		return nil, fmt.Errorf("tabula: a declared output and row source are required")
	}
	aggregate, err := checkRowGrouping(src.rowSource, project, spec.GroupBy)
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
	p := &RowsPlan[T, S]{view: out, lock: spec.Lock, withExists: spec.WithExists,
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
	forms, err := p.page.forms()
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
	p.family, err = compileRowFamily(&out.descriptor, name, src.rowSource, spec.RowFilter, names,
		func(c *typedCompiler, form string, predicates []Pred) (string, StatementShape, error) {
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

func checkRowGrouping(src rowSource, project []Projection, groups []RowColumn) (bool, error) {
	grouped := make(map[string]bool)
	for _, g := range groups {
		if g.ref == nil || g.ref.source != src.rel || grouped[g.ref.name] {
			return false, fmt.Errorf("tabula: GROUP BY requires distinct fields of its source")
		}
		grouped[g.ref.name] = true
	}
	aggregate := len(groups) > 0
	var free []string
	for _, p := range project {
		err := checkRowExpr(p.part, func(node expressionPart) error {
			switch f := node.(type) {
			case rowAggregate:
				aggregate = true
			case *fieldRef:
				free = append(free, f.name)
			}
			return nil
		})
		if err != nil {
			return false, err
		}
	}
	for _, name := range free {
		if aggregate && !grouped[name] {
			return false, fmt.Errorf("tabula: projection reads ungrouped field %q", name)
		}
	}
	return aggregate, nil
}

func (p *RowsPlan[T, S]) render(c *typedCompiler, src rowSource, project []Projection, groups []RowColumn,
	aggregate bool, predicates []Pred, data rowDataForm, count bool,
) (string, StatementShape, error) {
	var selectSQL string
	var fields []ShapeField
	if count {
		selectSQL = `pg_catalog.count(*) AS "count"`
		fields = []ShapeField{{Name: "count", Type: Int8().id.qualified()}}
	} else {
		columns, result, err := c.project(p.view.entry, project)
		if err != nil {
			return "", StatementShape{}, err
		}
		selectSQL, fields = strings.Join(columns, ", "), result
	}
	terms, err := src.where(c, predicates)
	if err != nil {
		return "", StatementShape{}, err
	}
	if data.page == AfterPage {
		after, err := data.order.afterSQL(c)
		if err != nil {
			return "", StatementShape{}, err
		}
		terms = append(terms, after)
	}
	from := " FROM " + src.fromSQL() + whereSQL(terms)
	if len(groups) > 0 {
		parts := make([]string, len(groups))
		for i, g := range groups {
			part, err := g.ref.render(c)
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
	if !count {
		order, err := data.order.orderSQL(c)
		if err != nil {
			return "", StatementShape{}, err
		}
		page, err := p.page.pageSQL(c, data.page)
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
	o := p.page.orders[order]
	if !p.page.pages.Keyset || o == nil || len(values) != len(o.terms) {
		return RowCursor{}, fmt.Errorf("tabula: unknown keyset order or incorrect cursor arity")
	}
	bindings := make([]Binding, len(values))
	for i, v := range values {
		f := o.terms[i].field
		if v.field == nil || v.field.source != f.source || v.field.name != f.name ||
			!sameSQLType(v.field.kind, f.kind) || v.value == nil && !f.nullable {
			return RowCursor{}, fmt.Errorf("tabula: incompatible cursor component %d", i+1)
		}
		bindings[i] = Binding{o.cursors[i], v.value}
	}
	return RowCursor{o, bindings}, nil
}
