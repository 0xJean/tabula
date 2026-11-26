package tabula

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ReadListSpec adds a finite list family to an explicit read. Budget covers
// every fixed, bounded and list shape, across all filters and scopes. Zero
// selects 256. ExactCount enables Count and paired List calls.
type ReadListSpec struct {
	Optional   []RowPredicate
	Orders     []RowOrder
	Pages      RowPages
	ExactCount bool
	Budget     int
}

// WithList adds separately selectable pages and optional exact summaries.
// Find and the bounded helpers retain the original fixed query. A repeated
// WithList, duplicate option name or exceeded budget fails at definition.
func (q ReadSelect) WithList(spec ReadListSpec) ReadSelect {
	q.listRepeated = q.list != nil
	spec.Optional = slices.Clone(spec.Optional)
	spec.Orders = slices.Clone(spec.Orders)
	for i := range spec.Orders {
		spec.Orders[i].Terms = slices.Clone(spec.Orders[i].Terms)
	}
	q.list = &spec
	return q
}

type readListPlan struct {
	page  rowPagePlan
	data  map[string]rowDataForm
	forms []string
	exact bool
}

// Optional predicates apply only to list forms. Fixed forms have one key per
// scope even when the list declares many independent filter choices.
func fixedReadChoices(path []shapeAlternative) bool {
	if len(path) == 0 || path[len(path)-1].choice != "form" || strings.HasPrefix(path[len(path)-1].value, "list.") {
		return true
	}
	for _, choice := range path {
		if strings.HasPrefix(choice.choice, "filter.") && choice.value == "on" {
			return false
		}
	}
	return true
}

func compileReadList(q ReadSelect, sources []rowSource) (*readListPlan, error) {
	if q.list == nil {
		return nil, nil
	}
	if q.listRepeated {
		return nil, fmt.Errorf("tabula: WithList may be declared only once")
	}
	spec := q.list
	if spec.ExactCount && q.lock.declared {
		return nil, fmt.Errorf("tabula: exact counts require an unlocked read")
	}
	p := &readListPlan{exact: spec.ExactCount, page: rowPagePlan{pages: spec.Pages, orders: make(map[string]*rowOrdering)}}
	for _, order := range spec.Orders {
		if !rowLabel(order.Name) || len(order.Terms) == 0 || p.page.orders[order.Name] != nil {
			return nil, fmt.Errorf("tabula: invalid or duplicate list order %q", order.Name)
		}
		o := &rowOrdering{name: order.Name, terms: slices.Clone(order.Terms)}
		for i, term := range o.terms {
			value := term.orderValue()
			if len(q.group) == 0 && !readHasAggregate(q) && hasAggregate(value.part) {
				return nil, fmt.Errorf("tabula: list order cannot introduce aggregation")
			}
			if value.part == nil || !readComparable(value.kind) {
				return nil, fmt.Errorf("tabula: order %q requires a supported comparable expression", order.Name)
			}
			for _, prior := range o.terms[:i] {
				if sameExpression(value.part, prior.orderValue().part) {
					return nil, fmt.Errorf("tabula: duplicate term in order %q", order.Name)
				}
			}
		}
		if spec.Pages.Keyset || len(q.distinctOn) != 0 {
			if err := prepareOrderCursor(o, q.project, readOuter(sources, q)); err != nil {
				return nil, err
			}
		}
		if spec.Pages.Keyset {
			_, _, err := readResultProof(q, sources, o.terms)
			if err != nil {
				return nil, fmt.Errorf("tabula: keyset order %q: %w", order.Name, err)
			}
		}
		p.page.orders[order.Name] = o
	}
	data, err := p.page.forms(singleRead{})
	if err != nil {
		return nil, err
	}
	p.data = make(map[string]rowDataForm, len(data))
	for _, name := range slices.Sorted(maps.Keys(data)) {
		p.data["list."+name] = data[name]
		p.forms = append(p.forms, "list."+name)
	}
	if p.exact {
		if err := q.checkCountStability(); err != nil {
			return nil, err
		}
		p.forms = append(p.forms, "list.count")
	}
	return p, nil
}

func readOuter(sources []rowSource, q ReadSelect) map[*occurrence]bool {
	outer := make(map[*occurrence]bool)
	for i, join := range q.joins {
		if join.kind == "LEFT JOIN" {
			outer[sources[i+1].rel] = true
		}
	}
	return outer
}

func (q ReadSelect) checkCountStability() error {
	var parts []expressionPart
	for _, p := range q.where {
		parts = append(parts, p.part)
	}
	for _, p := range q.having {
		parts = append(parts, p.part)
	}
	for _, p := range q.list.Optional {
		parts = append(parts, p.When.part)
	}
	for _, join := range q.joins {
		if join.kind != "CROSS JOIN" {
			parts = append(parts, join.on.part)
		}
	}
	for _, g := range q.group {
		if !isNilBound(g) {
			parts = append(parts, g.groupOperand().part)
		}
	}
	if q.distinct {
		for _, p := range q.project {
			parts = append(parts, p.part)
		}
	}
	for _, v := range q.distinctOn {
		parts = append(parts, v.part)
	}
	for _, part := range parts {
		if expressionHasVolatile(part) {
			return fmt.Errorf("tabula: exact count cardinality cannot depend on a volatile expression")
		}
	}
	return nil
}

func expressionHasVolatile(part expressionPart) bool {
	traits, ok := expressionStructure(part)
	if !ok || traits.stability == expressionVolatile {
		return true
	}
	return slices.ContainsFunc(traits.children, expressionHasVolatile)
}
