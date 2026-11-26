package tabula

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// RowTerm orders one declared typed expression. PostgreSQL orders NULLs last for
// ascending terms and first for descending terms.
type RowTerm struct {
	field *fieldRef
	desc  bool
	expr  scalarOperand
	nulls nullPlacement
}

type nullPlacement uint8

const (
	defaultNulls nullPlacement = iota
	firstNulls
	lastNulls
)

func (t RowTerm) orderValue() scalarOperand {
	if f := t.field; f != nil {
		return scalarOperand{f, f.kind, f.nullable}
	}
	return t.expr
}

func (t RowTerm) nullsFirst() bool {
	return t.nulls == firstNulls || t.nulls == defaultNulls && t.desc
}

// Asc orders a field from least to greatest.
func Asc[V any](f Field[V]) RowTerm { return RowTerm{field: f.ref} }

// Desc orders a field from greatest to least.
func Desc[V any](f Field[V]) RowTerm { return RowTerm{field: f.ref, desc: true} }

// AscExpr orders a typed expression from least to greatest.
func AscExpr[V any](value Expr[V]) RowTerm { return RowTerm{expr: value.scalarOperand()} }

// DescExpr orders a typed expression from greatest to least.
func DescExpr[V any](value Expr[V]) RowTerm { return RowTerm{expr: value.scalarOperand(), desc: true} }

// NullsFirst places SQL NULL before non-NULL values, independently of direction.
func (t RowTerm) NullsFirst() RowTerm { t.nulls = firstNulls; return t }

// NullsLast places SQL NULL after non-NULL values, independently of direction.
func (t RowTerm) NullsLast() RowTerm { t.nulls = lastNulls; return t }

// RowOrder names one complete ordering, including its tie-breakers.
type RowOrder struct {
	Name  string
	Terms []RowTerm
}

// RowPages declares the allowed page forms and their inclusive bounds. Its
// zero value registers only an unpaged read. Keyset registers first and after.
type RowPages struct {
	Unpaged, Offset, Keyset bool
	MaxLimit, MaxOffset     int64
}

func (p RowPages) includesUnpaged() bool { return p.Unpaged || !p.Offset && !p.Keyset }

// RowPage selects a registered page form.
type RowPage uint8

const (
	// Unpaged returns the entire filtered result.
	Unpaged RowPage = iota
	// OffsetPage binds Limit and Offset.
	OffsetPage
	// FirstPage binds Limit and starts at the beginning of a keyset order.
	FirstPage
	// AfterPage binds Limit and the cursor of the same plan and order.
	AfterPage
)

// RowChoice selects precompiled alternatives. Filters lists the enabled
// optional predicates. Order names a declared ordering.
type RowChoice struct {
	Filters       []string
	Order         string
	Page          RowPage
	Limit, Offset int64
	After         RowCursor
}

// CursorValue is one typed field value at a keyset boundary.
type CursorValue struct {
	field *fieldRef
	value any
	err   error
	expr  scalarOperand
}

// At supplies one cursor component using the field's input codec.
func At[V any](field Field[V], value V) CursorValue {
	b := field.kind.id.bind(nil, value)
	return CursorValue{field: field.ref, value: b.value, err: b.err}
}

// NullAt supplies a SQL NULL boundary for a nullable order field.
func NullAt[V any](field Field[V]) CursorValue { return CursorValue{field: field.ref} }

// RowCursor is a boundary issued for one plan and order. Its zero value
// denotes the absence of a cursor.
type RowCursor struct {
	order    *rowOrdering
	bindings []Binding
}

type rowOrdering struct {
	name              string
	terms             []RowTerm
	cursors           []*typedInput
	projectionIndexes []int
	nullable          []bool
}

func newRowOrder(order RowOrder, src rowSource, groups []RowColumn, aggregate, keyset bool, projection []Projection) (*rowOrdering, error) {
	if !rowLabel(order.Name) || len(order.Terms) == 0 {
		return nil, fmt.Errorf("tabula: a named row order requires at least one expression")
	}
	o := &rowOrdering{name: order.Name, terms: slices.Clone(order.Terms)}
	group := rowGroupOperands(groups)
	for i, term := range o.terms {
		value := term.orderValue()
		if err := validateOrderTerm(term); err != nil {
			return nil, err
		}
		if err := checkExpressionScope(value.part, map[*occurrence]bool{src.rel: true}); err != nil {
			return nil, err
		}
		for _, earlier := range o.terms[:i] {
			if sameExpression(earlier.orderValue().part, value.part) {
				return nil, fmt.Errorf("tabula: duplicate order expression")
			}
		}
		if aggregate {
			if err := groupDetermined(value.part, group); err != nil {
				return nil, fmt.Errorf("tabula: ungrouped order: %w", err)
			}
		}
	}
	if keyset {
		if aggregate && len(groups) == 0 {
			return nil, fmt.Errorf("tabula: scalar aggregate cannot have a keyset order")
		}
		if err := prepareOrderCursor(o, projection, nil); err != nil {
			return nil, err
		}
		if len(groups) > 0 {
			for _, g := range group {
				if !orderIncludes(o.terms, g.part) {
					return nil, fmt.Errorf("tabula: keyset order needs the complete group key")
				}
			}
		} else {
			if len(o.terms) < len(src.keys) {
				return nil, fmt.Errorf("tabula: keyset order %q needs the complete key as a tie-breaker", order.Name)
			}
			for _, name := range src.keys {
				if !slices.ContainsFunc(o.terms[len(o.terms)-len(src.keys):], func(t RowTerm) bool {
					f, ok := t.orderValue().part.(*fieldRef)
					return ok && f.source == src.rel && f.name == name
				}) {
					return nil, fmt.Errorf("tabula: incomplete keyset tie-breaker in order %q", order.Name)
				}
			}
		}
	}
	return o, nil
}

func orderIncludes(terms []RowTerm, part expressionPart) bool {
	return slices.ContainsFunc(terms, func(t RowTerm) bool { return sameExpression(t.orderValue().part, part) })
}

func validateOrderTerm(t RowTerm) error {
	v := t.orderValue()
	if t.nulls > lastNulls || !groupOrderComparable(v) {
		return fmt.Errorf("tabula: ORDER BY requires a comparable native type, got %s", v.kind.qualified())
	}
	return checkRowExpr(v.part, func(expressionPart) error { return nil })
}

func orderDirection(t RowTerm) string {
	direction := " ASC"
	if t.desc {
		direction = " DESC"
	}
	if t.nullsFirst() {
		return direction + " NULLS FIRST"
	}
	return direction + " NULLS LAST"
}

func (o *rowOrdering) orderSQL(c *typedCompiler) (string, error) {
	if o == nil {
		return "", nil
	}
	parts := make([]string, len(o.terms))
	for i, term := range o.terms {
		if err := validateOrderTerm(term); err != nil {
			return "", err
		}
		field, err := term.orderValue().part.render(c)
		if err != nil {
			return "", err
		}
		parts[i] = field + orderDirection(term)
	}
	return " ORDER BY " + strings.Join(parts, ", "), nil
}

func (o *rowOrdering) afterSQL(c *typedCompiler) (string, error) {
	var prefixes []string
	for i, term := range o.terms {
		if err := validateOrderTerm(term); err != nil {
			return "", err
		}
		field, err := term.orderValue().part.render(c)
		if err != nil {
			return "", err
		}
		slot, err := o.cursors[i].render(c)
		if err != nil {
			return "", err
		}
		greater := field + " > " + slot
		if term.desc {
			greater = field + " < " + slot
		}
		if o.termNullable(i) {
			nulls := field + " IS NULL AND " + slot + " IS NOT NULL"
			if term.nullsFirst() {
				nulls = field + " IS NOT NULL AND " + slot + " IS NULL"
			}
			greater = "(" + greater + " OR (" + nulls + "))"
		}
		if i == len(o.terms)-1 {
			return strings.Join(prefixes, "") + "(" + greater + ")" + strings.Repeat("))", i), nil
		}
		prefixes = append(prefixes, "("+greater+" OR ("+field+" IS NOT DISTINCT FROM "+slot+" AND ")
	}
	return "", fmt.Errorf("tabula: empty keyset order")
}

type rowDataForm struct {
	order *rowOrdering
	page  RowPage
	take  *typedInput
}

func orderedForm(form, order string) string {
	if order != "" {
		return form + ".sort." + order
	}
	return form
}

func dataForm(order string, page RowPage) string {
	// A runtime request only uses this label as a lookup key.
	form := orderedForm("data", order)
	switch page {
	case Unpaged:
		return form
	case FirstPage:
		return form + ".first"
	case AfterPage:
		return form + ".after"
	case OffsetPage:
		return form + ".offset"
	default:
		return ""
	}
}

type rowPagePlan struct {
	pages         RowPages
	orders        map[string]*rowOrdering
	limit, offset Slot[int64]
}

func (p *rowPagePlan) checkOrder(name string) error {
	if (name == "") != (len(p.orders) == 0) || name != "" && p.orders[name] == nil {
		return fmt.Errorf("tabula: unregistered row order %q", name)
	}
	return nil
}

func (p *rowPagePlan) forms(single singleRead) (map[string]rowDataForm, error) {
	if p.pages.MaxLimit < 0 || p.pages.MaxOffset < 0 ||
		(p.pages.Offset || p.pages.Keyset) != (p.pages.MaxLimit > 0) || !p.pages.Offset && p.pages.MaxOffset != 0 {
		return nil, fmt.Errorf("tabula: paged plans need a positive maximum limit and valid offset bounds")
	}
	p.limit, p.offset = Input("@limit", Int8()), Input("@offset", Int8())
	forms := make(map[string]rowDataForm)
	orders := slices.Sorted(maps.Keys(p.orders))
	if len(orders) == 0 {
		if p.pages.Offset || p.pages.Keyset {
			return nil, fmt.Errorf("tabula: pagination requires a declared order")
		}
		orders = []string{""}
	}
	for _, name := range orders {
		add := func(page RowPage) { forms[dataForm(name, page)] = rowDataForm{order: p.orders[name], page: page} }
		if single.take != nil {
			forms[orderedForm(single.form(), name)] = rowDataForm{order: p.orders[name], take: single.take}
		}
		if p.pages.includesUnpaged() {
			add(Unpaged)
		}
		if p.pages.Offset {
			add(OffsetPage)
		}
		if p.pages.Keyset {
			add(FirstPage)
			add(AfterPage)
		}
	}
	return forms, nil
}

func (p rowPagePlan) pageSQL(c *typedCompiler, data rowDataForm) (string, error) {
	if data.page == Unpaged {
		return renderSingleLimit(c, data.take)
	}
	limit, err := p.limit.input.render(c)
	if err != nil {
		return "", err
	}
	clause := " LIMIT " + limit
	if data.page == OffsetPage {
		offset, err := p.offset.input.render(c)
		if err != nil {
			return "", err
		}
		clause += " OFFSET " + offset
	}
	return clause, nil
}

func (p rowPagePlan) addPage(choice RowChoice, bindings []Binding) ([]Binding, error) {
	if choice.Limit > p.pages.MaxLimit || choice.Offset < 0 || choice.Offset > p.pages.MaxOffset ||
		(choice.Page != Unpaged && choice.Limit < 1) || (choice.Page == Unpaged && choice.Limit != 0) ||
		(choice.Page != OffsetPage && choice.Offset != 0) || (choice.Page == AfterPage) != (choice.After.order != nil) {
		return nil, fmt.Errorf("tabula: invalid row page or bound")
	}
	if choice.Page == AfterPage {
		if choice.After.order != p.orders[choice.Order] {
			return nil, fmt.Errorf("tabula: cursor belongs to a different plan or order")
		}
		bindings = append(bindings, choice.After.bindings...)
	}
	if choice.Page != Unpaged {
		bindings = append(bindings, BindValue(p.limit, choice.Limit))
	}
	if choice.Page == OffsetPage {
		bindings = append(bindings, BindValue(p.offset, choice.Offset))
	}
	return bindings, nil
}
