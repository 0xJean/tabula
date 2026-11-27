package plan

import (
	"github.com/0xJean/tabula"

	"fmt"
	"slices"
	"strings"
)

func (q ReadSelect) resultNode() statementNode { return q }
func (q ReadSelect) renderStatement(ctx *statementContext) (statementResult, error) {
	if ctx == nil || ctx.compiler == nil || ctx.block == nil {
		return statementResult{}, fmt.Errorf("tabula: query needs a statement context")
	}
	if ctx.position != statementRoot && (q.forms.Exact || q.forms.First != FirstDisabled || q.list != nil || q.unscoped) {
		return statementResult{}, fmt.Errorf("tabula: nested queries cannot contain executable families")
	}
	parent := ctx.block
	if ctx.position == statementRoot {
		parent = nil
	}
	block, err := newQueryBlock(parent, ctx.block.issuer)
	if err != nil {
		return statementResult{}, err
	}
	if ctx.position == statementRecursiveStep {
		block.recursive = ctx.block.recursive
	}
	c := ctx.compiler
	metadataStart := len(c.queryShapes)
	previous := c.block
	c.block = block
	defer func() { c.block = previous }()
	local := *ctx
	local.block = block
	prefix, effects, err := renderCommonTables(&local, q.tables)
	if err != nil {
		return statementResult{}, err
	}
	var result statementResult
	if q.set != nil {
		result, err = q.renderSet(&local)
	} else {
		result, err = q.renderQueryBlock(&local)
	}
	if err != nil {
		return statementResult{}, err
	}
	if block.recursive != nil && block.selfUses != 1 {
		return statementResult{}, fmt.Errorf("tabula: recursive step requires exactly one direct self reference")
	}
	result.effects, err = combineStatementEffects(result.effects, effects)
	if err != nil {
		return statementResult{}, err
	}
	result.sql = prefix + result.sql
	localMeta := result.shape
	result.shape, err = c.readRequirements()
	if err != nil {
		return statementResult{}, err
	}
	result.shape.AnyColumnUpdate = append(result.shape.AnyColumnUpdate, localMeta.AnyColumnUpdate...)
	result.shape.Keys = appendKeyRequirements(result.shape.Keys, localMeta.Keys...)
	result.shape.Comparisons = appendComparisonRequirements(result.shape.Comparisons, localMeta.Comparisons...)
	result.shape = mergeQueryShapeRange(c, result.shape, metadataStart)
	result.shape.Effects |= tabula.ShapeEffects(result.effects.Bits)
	result.effects.Bits = uint8(result.shape.Effects)
	result.shape.Result = queryShapeFields(result.fields)
	result.shape.ResultLineage = queryResultLineage(result.fields)
	return result.frozen(), nil
}

func (q ReadSelect) renderQueryBlock(ctx *statementContext) (statementResult, error) {
	c := ctx.compiler
	from, where, err := q.queryFrom(ctx)
	if err != nil {
		return statementResult{}, err
	}
	groups, aggregate, err := q.checkGrouping(c)
	if err != nil {
		return statementResult{}, err
	}
	if ctx.position == statementRecursiveStep && (aggregate || q.hasWindows() || q.distinct || len(q.distinctOn) > 0 || q.hasRowLocks()) {
		return statementResult{}, fmt.Errorf("tabula: recursive step forbids grouping, aggregates, distinct and row locks")
	}
	if err = q.checkDistinct(groups, aggregate, queryLocalSources(q)); err != nil {
		return statementResult{}, err
	}
	for _, part := range q.queryExpressions() {
		if err = checkQueryAggregateOwnership(part); err != nil {
			return statementResult{}, err
		}
		if err = checkQueryAggregateNulls(part, c.block); err != nil {
			return statementResult{}, err
		}
	}
	if err := q.checkRowLocks(c, aggregate, c.block.outerPhysical); err != nil {
		return statementResult{}, err
	}
	prefix, err := q.distinctSQL(c)
	if err != nil {
		return statementResult{}, err
	}
	selects, fields, err := c.projectQueryFields(ctx.output, q.project, ctx.indexes)
	if err != nil {
		return statementResult{}, err
	}
	sql := "SELECT " + prefix + strings.Join(selects, ", ")
	if from != "" {
		sql += " FROM " + from
	}
	sql += whereSQL(where)
	if len(groups) > 0 {
		parts := make([]string, len(groups))
		for i, g := range groups {
			parts[i], err = g.part.render(c)
			if err != nil {
				return statementResult{}, err
			}
		}
		sql += " GROUP BY " + strings.Join(parts, ", ")
	}
	if len(q.having) > 0 {
		parts := make([]string, len(q.having))
		for i, p := range q.having {
			parts[i], err = renderRowPredicate(c, p)
			if err != nil {
				return statementResult{}, err
			}
		}
		sql += " HAVING " + strings.Join(parts, " AND ")
	}
	window, err := q.windowClause(c)
	if err != nil {
		return statementResult{}, err
	}
	sql += window
	if len(q.order) > 0 {
		order, e := (&rowOrdering{terms: q.order}).orderSQL(c)
		if e != nil {
			return statementResult{}, e
		}
		sql += order
	}
	limit, err := q.limit.render(c)
	if err != nil {
		return statementResult{}, err
	}
	sql += limit
	meta := tabula.StatementShape{Effects: tabula.EffectRead}
	lock, err := q.rowLockClauses(c, &meta)
	if err != nil {
		return statementResult{}, err
	}
	sql += lock
	queryProveFields(c, q, fields)
	keys := queryProjectionKeys(c, q, fields)
	if len(q.distinctOn) > 0 {
		proof, err := queryWinnerRequirements(c, q)
		if err != nil {
			return statementResult{}, err
		}
		meta.Keys, meta.Comparisons = proof.keys, proof.comparisons
		for i := range keys {
			keys[i].keys = appendKeyRequirements(keys[i].keys, proof.keys...)
			keys[i].comparisons = appendComparisonRequirements(keys[i].comparisons, proof.comparisons...)
		}
		for i := range fields {
			fields[i].proof = fields[i].proof.combine(proof)
		}
	}
	return statementResult{sql: sql, fields: fields, keys: keys, shape: meta, effects: statementEffects{Bits: uint8(meta.Effects)}}, nil
}

// renderQuerySource makes one occurrence visible. A physical source's scope
// and liveness predicates remain the caller's responsibility at its join edge.
func renderQuerySource(ctx *statementContext, source ReadSource, lateral bool) (string, []queryResultField, []queryResultKey, error) {
	c, b := ctx.compiler, ctx.block
	if isNilBound(source) {
		return "", nil, nil, fmt.Errorf("tabula: undeclared query source")
	}
	if result, ok := source.(QuerySource); ok {
		occurrence := result.occurrence
		if occurrence == nil || isNilBound(occurrence.node) {
			return "", nil, nil, fmt.Errorf("tabula: undeclared result source")
		}
		parent := b
		if !lateral {
			var err error
			parent, err = queryBarrier(b)
			if err != nil {
				return "", nil, nil, err
			}
		}
		if _, unnest := occurrence.node.(unnestQuery); unnest {
			var err error
			parent, err = newQueryBlock(parent, parent.issuer)
			if err != nil {
				return "", nil, nil, err
			}
		}
		child := &statementContext{compiler: c, block: parent, position: statementDerived}
		old := c.block
		c.block = parent
		rendered, err := occurrence.node.renderStatement(child)
		c.block = old
		if err != nil {
			return "", nil, nil, err
		}
		if tabula.ShapeEffects(rendered.effects.Bits)&tabula.EffectWrite != 0 {
			if _, common := occurrence.node.(commonTableReference); !common {
				return "", nil, nil, fmt.Errorf("tabula: derived queries cannot contain write effects")
			}
		}
		if _, common := occurrence.node.(commonTableReference); !common {
			c.queryShapes = append(c.queryShapes, rendered.shape)
		}
		if err = b.addResult(occurrence); err != nil {
			return "", nil, nil, err
		}
		b.resultFields[occurrence] = cloneQueryFields(rendered.fields)
		b.resultKeys[occurrence] = cloneQueryKeys(rendered.keys)
		alias := c.queryAlias()
		b.resultAliases[occurrence] = alias
		sql := rendered.sql
		switch node := occurrence.node.(type) {
		case commonTableReference:
			if node.self && b.recursive != node.common {
				return "", nil, nil, fmt.Errorf("tabula: recursive self reference escaped its step")
			}
			if node.common.seed != nil {
				if b.recursive == node.common {
					b.selfUses++
				} else if queryRecursiveActive(b, node.common) {
					return "", nil, nil, fmt.Errorf("tabula: recursive self reference is not a direct step source")
				}
			}
		case unnestQuery:
			var names []string
			var ordinal string
			for i, f := range rendered.fields {
				if node.columns[i].ordinal {
					ordinal = typedQuote(f.name)
				} else {
					names = append(names, typedQuote(f.name))
				}
			}
			if ordinal != "" {
				names = append(names, ordinal)
			}
			return sql + " AS " + typedQuote(alias) + "(" + strings.Join(names, ", ") + ")", rendered.fields, rendered.keys, nil
		default:
			sql = "(" + sql + ")"
		}
		return sql + " AS " + typedQuote(alias), rendered.fields, rendered.keys, nil
	}
	src, err := source.readSource()
	if err != nil {
		return "", nil, nil, err
	}
	if err = b.addPhysical(src.rel); err != nil {
		return "", nil, nil, err
	}
	alias := c.queryAlias()
	b.physicalAliases[src.rel] = alias
	if c.queryReads != nil {
		c.queryReads[src.rel.table] = true
	}
	return src.rel.table.SQL() + " AS " + typedQuote(alias), nil, nil, nil
}
func queryRecursiveActive(b *queryBlock, n *commonTableNode) bool {
	for ; b != nil; b = b.parent {
		if b.recursive == n {
			return true
		}
	}
	return false
}

func (q ReadSelect) queryFrom(ctx *statementContext) (string, []string, error) {
	var from string
	var where []string
	c := ctx.compiler
	if q.from != nil {
		var err error
		from, _, _, err = renderQuerySource(ctx, q.from, false)
		if err != nil {
			return "", nil, err
		}
		terms, e := querySourcePredicates(c, q.from)
		if e != nil {
			return "", nil, e
		}
		where = append(where, terms...)
	} else if len(q.joins) > 0 {
		return "", nil, fmt.Errorf("tabula: JOIN requires an initial source")
	}
	for _, join := range q.joins {
		right, _, _, err := renderQuerySource(ctx, join.source, queryIsLateral(join.kind))
		if err != nil {
			return "", nil, err
		}
		from += " " + join.kind + " " + right
		terms, err := querySourcePredicates(c, join.source)
		if err != nil {
			return "", nil, err
		}
		if strings.HasPrefix(join.kind, "CROSS JOIN") {
			where = append(where, terms...)
		} else {
			if err = checkRowExpr(join.on.part, nil); err != nil {
				return "", nil, err
			}
			on, err := renderRowPredicate(c, join.on)
			if err != nil {
				return "", nil, err
			}
			from += " ON " + strings.Join(append([]string{on}, terms...), " AND ")
		}
		if strings.HasPrefix(join.kind, "LEFT JOIN") {
			if result, ok := join.source.(QuerySource); ok && result.occurrence != nil {
				if ref, ok := result.occurrence.node.(commonTableReference); ok && ref.common == ctx.block.recursive {
					return "", nil, fmt.Errorf("tabula: recursive self cannot be on a nullable join side")
				}
			}
			if result, ok := join.source.(QuerySource); ok {
				ctx.block.outerResults[result.occurrence] = true
			} else {
				src, _ := join.source.readSource()
				ctx.block.outerPhysical[src.rel] = true
			}
		}
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
	return from, where, nil
}
func querySourcePredicates(c *typedCompiler, source ReadSource) ([]string, error) {
	if _, result := source.(QuerySource); result {
		return nil, nil
	}
	src, err := source.readSource()
	if err != nil {
		return nil, err
	}
	return src.where(c, nil)
}
func queryLocalSources(q ReadSelect) []rowSource {
	var out []rowSource
	for _, source := range append([]ReadSource{q.from}, queryJoinSources(q.joins)...) {
		if source == nil {
			continue
		}
		if _, result := source.(QuerySource); result {
			continue
		}
		src, err := source.readSource()
		if err == nil {
			out = append(out, src)
		}
	}
	return out
}
func queryJoinSources(joins []readJoin) []ReadSource {
	out := make([]ReadSource, len(joins))
	for i, j := range joins {
		out[i] = j.source
	}
	return out
}

// InnerLateral joins a result whose Outer references see preceding inputs only.
func (q ReadSelect) InnerLateral(source QuerySource, on Pred) ReadSelect {
	q.joins = append(slices.Clip(q.joins), readJoin{"INNER JOIN LATERAL", source, on})
	return q
}

// LeftLateral preserves left rows and makes an empty right result nullable.
func (q ReadSelect) LeftLateral(source QuerySource, on Pred) ReadSelect {
	q.joins = append(slices.Clip(q.joins), readJoin{"LEFT JOIN LATERAL", source, on})
	return q
}

// CrossLateral declares a lateral product. No per-row execution is promised
// unless the source has an actual explicit outer dependency.
func (q ReadSelect) CrossLateral(source QuerySource) ReadSelect {
	q.joins = append(slices.Clip(q.joins), readJoin{kind: "CROSS JOIN LATERAL", source: source})
	return q
}
