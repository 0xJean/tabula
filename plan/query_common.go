package plan

import (
	"github.com/0xJean/tabula"

	"fmt"
	"slices"
	"strings"
)

// Materialization selects PostgreSQL's explicit common-table strategy.
type Materialization uint8

const (
	// DefaultMaterialization uses PostgreSQL's normal planning behavior.
	DefaultMaterialization Materialization = iota
	// Materialized provides a stored result slot, without upgrading volatility.
	Materialized
	// NotMaterialized permits inlining and provides no repeatability proof.
	NotMaterialized
)

// RecursiveUnion selects the supported recursive combination.
type RecursiveUnion uint8

const (
	// RecursiveUnionDistinct removes duplicates during recursive evaluation.
	RecursiveUnionDistinct RecursiveUnion = iota + 1
	// RecursiveUnionAll preserves recursive multiplicity.
	RecursiveUnionAll
)

// CommonTable is an immutable named query declaration. Attach it using With.
type CommonTable struct{ node *commonTableNode }
type commonTableNode struct {
	name       string
	query      statementNode
	mode       Materialization
	fields     []queryResultField
	seed, step *ReadSelect
	combine    RecursiveUnion
	err        error
}

// CTE declares a read common table without registering a separate statement.
func CTE(name string, query ReadSelect, mode Materialization) CommonTable {
	c := commonTableFromStatement(name, query)
	c.node.mode = mode
	return c
}
func commonTableFromStatement(name string, node statementNode) CommonTable {
	c := &commonTableNode{name: name, query: node}
	if q, ok := node.(interface{ queryDeclaredFields() []queryResultField }); ok {
		c.fields = q.queryDeclaredFields()
	}
	return CommonTable{c}
}

// As exposes the common table's exact exported fields under a new occurrence.
// An effect-only common table cannot be used as a result source.
func (c CommonTable) As(alias string) QuerySource {
	if c.node == nil {
		return QuerySource{}
	}
	fields := cloneQueryFields(c.node.fields)
	if c.node.mode == Materialized {
		for i := range fields {
			fields[i].stored = true
		}
	}
	return QuerySource{&queryOccurrence{alias: alias, node: commonTableReference{common: c.node}, fields: fields}}
}

// With attaches declarations in lexical dependency order. A later declaration
// may reference an earlier one; a name alone never supplies a binding.
func (q ReadSelect) With(tables ...CommonTable) ReadSelect {
	q.tables = slices.Concat(q.tables, tables)
	return q
}

// RecursiveCTE declares a bounded-by-query recursive relation. The self token
// is legal only once in its own step, with explicit termination predicates.
// SEARCH, CYCLE and native record path decorators are not exposed.
func RecursiveCTE(name string, seed ReadSelect, combine RecursiveUnion, step func(QuerySource) ReadSelect) CommonTable {
	c := commonTableFromStatement(name, seed)
	c.node.seed = &seed
	c.node.combine = combine
	if step == nil {
		c.node.err = fmt.Errorf("tabula: recursive step is required")
		return c
	}
	// Conservatively widen the self schema before the callback can capture it.
	for i := range c.node.fields {
		c.node.fields[i].value.nullable = true
	}
	self := c.As(name)
	self.occurrence.node = commonTableReference{common: c.node, self: true}
	next := step(self)
	c.node.step = &next
	return c
}

type commonTableReference struct {
	common *commonTableNode
	self   bool
}

func (r commonTableReference) statementSources() ([]rowSource, error) { return nil, nil }
func (r commonTableReference) renderStatement(ctx *statementContext) (statementResult, error) {
	for b := ctx.block; b != nil; b = b.parent {
		if result, ok := b.common[r.common]; ok {
			result = result.frozen()
			return result, nil
		}
	}
	return statementResult{}, fmt.Errorf("tabula: common table is not attached or is a forward dependency")
}
func commonTableSources(tables []CommonTable) ([]rowSource, error) {
	var result []rowSource
	for _, table := range tables {
		if table.node == nil || isNilBound(table.node.query) {
			return nil, fmt.Errorf("tabula: undeclared common table")
		}
		sources, err := table.node.query.statementSources()
		if err != nil {
			return nil, err
		}
		result = append(result, sources...)
		if table.node.step != nil {
			sources, err = table.node.step.statementSources()
			if err != nil {
				return nil, err
			}
			result = append(result, sources...)
		}
	}
	return uniqueQuerySources(result), nil
}
func renderCommonTables(ctx *statementContext, tables []CommonTable) (string, statementEffects, error) {
	effects := statementEffects{}
	var clauses []string
	names := make(map[string]bool)
	recursive := false
	for _, table := range tables {
		n := table.node
		if n == nil || !typedIdent(n.name) || names[n.name] || n.mode > NotMaterialized {
			return "", effects, fmt.Errorf("tabula: invalid or duplicate common table")
		}
		if n.err != nil {
			return "", effects, n.err
		}
		names[n.name] = true
		barrier, err := queryBarrier(ctx.block)
		if err != nil {
			return "", effects, err
		}
		// Containing-query FROM fields are invisible in its common-table body,
		// while explicit deeper Outer references retain already-visible ancestors.
		barrier.parent = ctx.block.parent
		child := &statementContext{compiler: ctx.compiler, block: barrier, position: statementCommonTable}
		var result statementResult
		name := ctx.compiler.queryAlias()
		if n.seed != nil {
			recursive = true
			result, err = n.renderRecursive(child, name)
		} else {
			result, err = n.query.renderStatement(child)
		}
		if err != nil {
			return "", effects, err
		}
		if tabula.ShapeEffects(result.effects.Bits)&tabula.EffectWrite != 0 {
			if ctx.position != statementRoot {
				return "", effects, fmt.Errorf("tabula: modifying common tables belong only to the statement root")
			}
			result.effects.WriteCTE = true
		}
		effects, err = combineStatementEffects(effects, result.effects)
		if err != nil {
			return "", effects, err
		}
		if n.mode == Materialized {
			for i := range result.fields {
				result.fields[i].stored = true
			}
		}
		binding := result.frozen()
		binding.sql = typedQuote(name)
		ctx.block.common[n] = binding
		ctx.compiler.queryShapes = append(ctx.compiler.queryShapes, result.shape)
		mode := ""
		if n.mode == Materialized {
			mode = "MATERIALIZED "
		}
		if n.mode == NotMaterialized {
			mode = "NOT MATERIALIZED "
		}
		clauses = append(clauses, typedQuote(name)+" AS "+mode+"("+result.sql+")")
	}
	if len(clauses) == 0 {
		return "", effects, nil
	}
	prefix := "WITH "
	if recursive {
		prefix += "RECURSIVE "
	}
	return prefix + strings.Join(clauses, ", ") + " ", effects, nil
}
func (n *commonTableNode) renderRecursive(ctx *statementContext, name string) (statementResult, error) {
	if n.step == nil || (n.combine != RecursiveUnionAll && n.combine != RecursiveUnionDistinct) {
		return statementResult{}, fmt.Errorf("tabula: unsupported recursive combination")
	}
	seed, err := n.seed.renderStatement(ctx)
	if err != nil {
		return statementResult{}, err
	}
	if tabula.ShapeEffects(seed.effects.Bits)&tabula.EffectWrite != 0 {
		return statementResult{}, fmt.Errorf("tabula: recursive seed must be read-only")
	}
	ctx.block.common[n] = statementResult{sql: typedQuote(name), fields: cloneQueryFields(n.fields), effects: statementEffects{Bits: uint8(tabula.EffectRead)}}
	ctx.block.recursive = n
	stepCtx := *ctx
	stepCtx.position = statementRecursiveStep
	step, err := n.step.renderStatement(&stepCtx)
	if err != nil {
		return statementResult{}, err
	}
	if err = matchQueryFields(seed.fields, step.fields); err != nil {
		return statementResult{}, err
	}
	if tabula.ShapeEffects(step.effects.Bits)&tabula.EffectWrite != 0 {
		return statementResult{}, fmt.Errorf("tabula: recursive step must be read-only")
	}
	op := " UNION "
	if n.combine == RecursiveUnionAll {
		op = " UNION ALL "
	}
	seed.sql = "(" + seed.sql + ")" + op + "(" + step.sql + ")"
	seed.keys = nil
	for i := range seed.fields {
		seed.fields[i].lineage = tabula.ResultLineage{}
		seed.fields[i].value.nullable = true
		seed.fields[i].stability = max(seed.fields[i].stability, step.fields[i].stability)
		seed.fields[i].origins = append(seed.fields[i].origins, step.fields[i].origins...)
		seed.fields[i].secret = seed.fields[i].secret || step.fields[i].secret
	}
	return seed, nil
}
func queryBarrier(parent *queryBlock) (*queryBlock, error) {
	if parent == nil {
		return nil, fmt.Errorf("tabula: query needs an enclosing registry")
	}
	barrier, err := newQueryBlock(nil, parent.issuer)
	if err != nil {
		return nil, err
	}
	// Copy lexical common tables without exposing any physical/result occurrence.
	var chain []*queryBlock
	for p := parent; p != nil; p = p.parent {
		chain = append(chain, p)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		for n, r := range chain[i].common {
			barrier.common[n] = r
		}
	}
	return barrier, nil
}
