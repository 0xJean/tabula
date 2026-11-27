package tabula

import (
	"fmt"
	"strconv"
	"strings"
)

func newQueryCompiler(scoped bool, issuer *statementIssuer, sources []rowSource) (*typedCompiler, error) {
	c, err := newTypedCompiler(false)
	if err != nil {
		return nil, err
	}
	c.params.graph = true
	c.queryReads = make(map[physicalName]bool)
	c.extendedForm = true
	for _, src := range sources {
		if src.rel == nil || src.rel.issuer != issuer {
			return nil, fmt.Errorf("tabula: query sources must belong to one registry")
		}
		if _, ok := c.sources[src.rel]; ok {
			continue
		}
		c.sources[src.rel] = false
		c.from = append(c.from, src.rel)
		if c.selectCols[src.rel.table] == nil {
			c.selectCols[src.rel.table] = make(map[string]bool)
		}
		c.params.scoped = c.params.scoped || scoped && src.rel.scope == ByScopeColumn()
	}
	return c, nil
}

func (l *parameterLayout) snapshotName(input *typedInput) string {
	if !l.graph {
		return input.name
	}
	// Every graph label carries its global position; even adversarially named
	// independent declarations remain unambiguous and values stay private.
	return "@" + strconv.Itoa(l.positions[input]+1) + ":" + input.name
}

func (q ReadSelect) composed() bool {
	if isNilBound(q.from) || q.set != nil || len(q.tables) > 0 || q.limit != nil {
		return true
	}
	if _, ok := q.from.(QuerySource); ok {
		return true
	}
	for _, j := range q.joins {
		if _, ok := j.source.(QuerySource); ok {
			return true
		}
	}
	for _, p := range q.queryExpressions() {
		if queryExpressionComposed(p) {
			return true
		}
	}
	return false
}
func queryExpressionComposed(p expressionPart) bool {
	switch p.(type) {
	case queryExpression, queryOuter, queryResultRef:
		return true
	}
	t, _ := expressionStructure(p)
	for _, child := range t.children {
		if queryExpressionComposed(child) {
			return true
		}
	}
	return false
}

func compileQueryPlan[T, S any](owner *descriptor[T, S], indexes []int, name string, q ReadSelect, observe []StatementOption) (*ReadPlan[T, S], error) {
	if owner == nil {
		return nil, fmt.Errorf("tabula: DefineRead requires a registered output")
	}
	sources, err := q.statementSources()
	if err != nil {
		return nil, err
	}
	single, err := newSingleRead(q.forms, len(q.order) > 0, !q.hasRowLocks())
	if err != nil {
		return nil, err
	}
	forms := []string{"data"}
	if single.take != nil {
		forms = append(forms, single.form())
	}
	list, err := compileReadList(q, sources)
	if err != nil {
		return nil, err
	}
	spec := RowFilter{AllowUnscoped: q.unscoped, Observe: observe}
	var reachable func([]shapeAlternative) bool
	if list != nil {
		spec.Optional, spec.Budget = q.list.Optional, q.list.Budget
		forms = append(forms, list.forms...)
		reachable = fixedReadChoices
	}
	family, err := compileSourceFamily(owner, name, sources, spec, forms, reachable, true,
		func(c *typedCompiler, form string, predicates []Pred) (string, StatementShape, error) {
			root, err := newQueryBlock(nil, owner.reg.statementIssuer)
			if err != nil {
				return "", StatementShape{}, err
			}
			ctx := &statementContext{compiler: c, block: root, position: statementRoot, output: owner.entry, indexes: indexes}
			if strings.HasPrefix(form, "list.") {
				return list.renderQuery(ctx, q.Where(predicates...), form)
			}
			result, err := q.renderStatement(ctx)
			if err != nil {
				return "", StatementShape{}, err
			}
			if result.effects.bits&EffectWrite != 0 {
				return "", StatementShape{}, fmt.Errorf("tabula: DefineRead cannot register write effects")
			}
			if form != "data" {
				limit, e := renderSingleLimit(c, single.take)
				if e != nil {
					return "", StatementShape{}, e
				}
				result.sql = "SELECT * FROM (" + result.sql + ") AS \"result\"" + limit
			}
			return result.sql, result.shape, nil
		})
	if err != nil {
		return nil, err
	}
	for _, shape := range family.shapes {
		if shape.shape.Effects&EffectRowLock != 0 {
			single.unbounded = false
		}
	}
	return &ReadPlan[T, S]{owner: owner, indexes: indexes, family: family, single: single, list: list}, nil
}

func (q ReadSelect) queryExpressions() []expressionPart {
	var parts []expressionPart
	for _, p := range q.project {
		parts = append(parts, p.part)
	}
	for _, p := range q.where {
		parts = append(parts, p.part)
	}
	for _, p := range q.having {
		parts = append(parts, p.part)
	}
	for _, j := range q.joins {
		if j.on.part != nil {
			parts = append(parts, j.on.part)
		}
	}
	for _, g := range q.group {
		if !isNilBound(g) {
			parts = append(parts, g.groupOperand().part)
		}
	}
	for _, o := range q.order {
		parts = append(parts, o.orderValue().part)
	}
	for _, d := range q.distinctOn {
		parts = append(parts, d.part)
	}
	return parts
}

func (q ReadSelect) statementSources() ([]rowSource, error) {
	sources, err := commonTableSources(q.tables)
	if err != nil {
		return nil, err
	}
	if q.set != nil {
		for _, arm := range q.set.arms {
			rows, e := arm.statementSources()
			if e != nil {
				return nil, e
			}
			sources = append(sources, rows...)
		}
	}
	if q.from != nil {
		rows, e := querySourceLeaves(q.from)
		if e != nil {
			return nil, e
		}
		sources = append(sources, rows...)
	}
	for _, j := range q.joins {
		rows, e := querySourceLeaves(j.source)
		if e != nil {
			return nil, e
		}
		sources = append(sources, rows...)
	}
	rows, err := queryExpressionSources(q.queryExpressions()...)
	return uniqueQuerySources(append(sources, rows...)), err
}
func querySourceLeaves(source ReadSource) ([]rowSource, error) {
	if isNilBound(source) {
		return nil, fmt.Errorf("tabula: missing query source")
	}
	if q, ok := source.(QuerySource); ok {
		if q.occurrence == nil || isNilBound(q.occurrence.node) {
			return nil, fmt.Errorf("tabula: undeclared query source")
		}
		return q.occurrence.node.statementSources()
	}
	row, err := source.readSource()
	return []rowSource{row}, err
}
func queryExpressionSources(parts ...expressionPart) ([]rowSource, error) {
	var result []rowSource
	for _, p := range parts {
		if q, ok := p.(queryExpression); ok {
			rows, err := q.query.statementSources()
			if err != nil {
				return nil, err
			}
			result = append(result, rows...)
		}
		traits, _ := expressionStructure(p)
		rows, err := queryExpressionSources(traits.children...)
		if err != nil {
			return nil, err
		}
		result = append(result, rows...)
	}
	return uniqueQuerySources(result), nil
}
func uniqueQuerySources(sources []rowSource) []rowSource {
	seen := make(map[*occurrence]bool)
	result := make([]rowSource, 0, len(sources))
	for _, src := range sources {
		if !seen[src.rel] {
			seen[src.rel] = true
			result = append(result, src)
		}
	}
	return result
}
func (c *typedCompiler) queryAlias() string {
	alias := "_tabula_" + strconv.Itoa(c.querySerial)
	c.querySerial++
	return alias
}

func queryShapeFields(fields []queryResultField) []ShapeField {
	result := make([]ShapeField, len(fields))
	for i, f := range fields {
		result[i] = ShapeField{Name: f.name, Type: f.value.kind.qualified(), Nullable: f.value.nullable}
	}
	return result
}
func queryIsLateral(kind string) bool { return strings.HasSuffix(kind, " LATERAL") }
