package plan

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/0xJean/tabula"
)

// WritePlan is a registered count-only write. Binding never changes its SQL.
type WritePlan[S any] struct {
	family   *rowFamily
	batch    *writeBatch
	bind     func(tabula.Session[S]) *BoundWritePlan[S]
	bindCall func(tabula.Session[S], *compiledShape, tabula.CallMethod, []Binding) (tabula.AuthorizedStatement, error)
	validate func(tabula.Session[S], *compiledShape, tabula.CallMethod, []Binding) error
}

// DefineWrite registers a write owned by its outer physical target. It panics
// on declaration errors; the Try partner publishes nothing on failure.
func DefineWrite[T, S any](owner RowSource[T, S], name string, query WriteStatement, observe ...tabula.StatementOption) *WritePlan[S] {
	p, err := TryDefineWrite(owner, name, query, observe...)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineWrite atomically registers all declared scope and batch forms.
func TryDefineWrite[T, S any](owner RowSource[T, S], name string, query WriteStatement, observe ...tabula.StatementOption) (*WritePlan[S], error) {
	if owner.owner == nil || isNilBound(query) {
		return nil, fmt.Errorf("tabula: DefineWrite requires a declared owner and write")
	}
	node, ok := query.writeNode().(writeDeclaration)
	if !ok || node.target.rel != owner.rel || len(node.returning) != 0 {
		return nil, fmt.Errorf("tabula: DefineWrite owner must be the outer physical target")
	}
	nodes, batch, err := writeForms(node)
	if err != nil {
		return nil, err
	}
	family, err := compileWriteFamily(owner.owner, nil, name, nodes, node.unscoped, RowFilter{Observe: observe}, false)
	if err != nil {
		return nil, err
	}
	p := &WritePlan[S]{
		family: family,
		batch:  batch,
		bindCall: func(session tabula.Session[S], shape *compiledShape, method tabula.CallMethod, values []Binding) (tabula.AuthorizedStatement, error) {
			return bindShapeCall(owner.owner, shape, session, method, values, family.meta.Quiet, false)
		},
		validate: func(session tabula.Session[S], shape *compiledShape, method tabula.CallMethod, values []Binding) error {
			return validateWriteBindings(owner.owner, session, shape, method, values)
		},
	}
	p.bind = func(session tabula.Session[S]) *BoundWritePlan[S] {
		execution := bindRowExecution(owner.owner, family, session)
		return bindWritePlan(p, session, execution)
	}
	return p, nil
}

type writeResult struct{ node writeDeclaration }

func (q writeResult) resultNode() statementNode { return q.node }

// WithReturning adds one ordered result projection to a write. No read limit
// or extra execution is introduced. Exported fields support intermediate CTEs.
func WithReturning(query WriteStatement, fields ...Projection) ResultStatement {
	if isNilBound(query) {
		return writeResult{writeDeclaration{err: fmt.Errorf("tabula: RETURNING requires a write")}}
	}
	node, ok := query.writeNode().(writeDeclaration)
	if !ok {
		return writeResult{writeDeclaration{err: fmt.Errorf("tabula: unsupported RETURNING write")}}
	}
	node.returning = slices.Clone(fields)
	if len(fields) == 0 {
		node.err = fmt.Errorf("tabula: RETURNING requires at least one field")
	}
	return writeResult{node}
}

// WriteCTE declares a modifying common table whose RETURNING schema may be
// referenced by later common tables and the outer statement. Reuse preserves
// the same effect identity; independent writes to one physical table fail.
func WriteCTE(name string, query ResultStatement) CommonTable {
	if isNilBound(query) {
		return commonTableFromStatement(name, writeDeclaration{err: fmt.Errorf("tabula: WriteCTE requires a write result")})
	}
	node, ok := query.resultNode().(writeDeclaration)
	if !ok || len(node.returning) == 0 {
		return commonTableFromStatement(name, writeDeclaration{err: fmt.Errorf("tabula: WriteCTE requires a modifying RETURNING statement")})
	}
	node.id = new(statementEffectIdentity)
	return commonTableFromStatement(name, node)
}

// EffectCTE declares an effect-only modifying common table. It executes even
// when unreferenced and cannot be used as a FROM source.
func EffectCTE(name string, query WriteStatement) CommonTable {
	if isNilBound(query) {
		return commonTableFromStatement(name, writeDeclaration{err: fmt.Errorf("tabula: EffectCTE requires a write")})
	}
	node, ok := query.writeNode().(writeDeclaration)
	if !ok {
		return commonTableFromStatement(name, writeDeclaration{err: fmt.Errorf("tabula: EffectCTE requires a modifying statement")})
	}
	node.id = new(statementEffectIdentity)
	return commonTableFromStatement(name, node)
}

func compileWriteFamily[T, S any](owner *descriptor[T, S], indexes []int, name string,
	nodes map[string]statementNode, unscoped bool, spec RowFilter, returning bool,
) (*rowFamily, error) {
	if owner == nil || owner.Entry == nil || !rowLabel(name) || len(nodes) == 0 {
		return nil, fmt.Errorf("tabula: a registered owner and named write family are required")
	}
	family := &rowFamily{scope: tabula.Global(), shapes: make(map[rowVariant]*compiledShape), meta: ownerStatementMeta(owner, "", spec.Observe)}
	forms := slices.Sorted(maps.Keys(nodes))
	var sources []rowSource
	seen := make(map[*occurrence]bool)
	for _, form := range forms {
		leaves, err := nodes[form].statementSources()
		if err != nil {
			return nil, err
		}
		for _, leaf := range leaves {
			if leaf.rel == nil || leaf.rel.issuer != owner.Issuer {
				return nil, fmt.Errorf("tabula: write sources must belong to one registry")
			}
			if !seen[leaf.rel] {
				seen[leaf.rel] = true
				sources = append(sources, leaf)
			}
		}
	}
	for _, predicate := range spec.Optional {
		leaves, err := queryExpressionSources(predicate.When.part)
		if err != nil {
			return nil, err
		}
		for _, leaf := range leaves {
			if leaf.rel == nil || leaf.rel.issuer != owner.Issuer {
				return nil, fmt.Errorf("tabula: optional write sources must belong to one registry")
			}
			if !seen[leaf.rel] {
				seen[leaf.rel] = true
				sources = append(sources, leaf)
			}
		}
	}
	var scopeType catalogType
	for _, src := range sources {
		if src.rel.scope == tabula.ByScopeColumn() {
			if scopeType.GoType != nil && (!sameSQLType(scopeType, src.scopeType) ||
				!owner.HasScopeID && scopeType.Encoder != src.scopeType.Encoder) {
				return nil, fmt.Errorf("tabula: write graph scope types or encoders disagree")
			}
			scopeType, family.scope = src.scopeType, tabula.ByScopeColumn()
		} else if src.rel.scope == tabula.ByRLS() && family.scope != tabula.ByScopeColumn() {
			family.scope = tabula.ByRLS()
		}
		if src.liveValue.input != nil && !slices.ContainsFunc(family.live, func(b Binding) bool { return b.input == src.liveValue.input }) {
			family.live = append(family.live, src.liveValue)
		}
	}
	scope := "global"
	if family.scope == tabula.ByScopeColumn() {
		scope = "scoped"
		if unscoped {
			scope = "unscoped"
		}
	} else if family.scope == tabula.ByRLS() {
		scope = "rls"
	}
	scopes := []string{scope}
	if scope == "scoped" && spec.AllowUnscoped {
		scopes = append(scopes, "unscoped")
	}
	choices := []shapeChoice{{"scope", scopes}}
	for _, pred := range spec.Optional {
		if !rowLabel(pred.Name) || pred.When.part == nil {
			return nil, fmt.Errorf("tabula: invalid optional mutation predicate")
		}
		family.filters = append(family.filters, pred.Name)
		choices = append(choices, shapeChoice{"filter." + pred.Name, []string{"off", "on"}})
	}
	choices = append(choices, shapeChoice{"form", forms})
	paths, err := enumerateShapes(name, spec.Budget, choices, nil)
	if err != nil {
		return nil, err
	}
	var shapes []*compiledShape
	for _, path := range paths {
		scope := path[0].value
		form := path[len(path)-1].value
		node := nodes[form]
		var filters []string
		for i, pred := range spec.Optional {
			if path[i+1].value == "on" {
				filters = append(filters, pred.Name)
				n, ok := node.(writeDeclaration)
				if !ok {
					return nil, fmt.Errorf("tabula: optional write predicates require a mutation")
				}
				n.where = append(slices.Clip(n.where), pred.When)
				node = n
			}
		}
		actual, err := node.statementSources()
		if err != nil {
			return nil, err
		}
		c, err := newQueryCompiler(scope == "scoped", owner.Issuer, actual)
		if err != nil {
			return nil, err
		}
		block, err := newQueryBlock(nil, owner.Issuer)
		if err != nil {
			return nil, err
		}
		ctx := &statementContext{compiler: c, block: block, position: statementRoot}
		if returning {
			ctx.output, ctx.indexes = ownerOutputLayout(owner), indexes
		}
		result, err := node.renderStatement(ctx)
		if err != nil {
			return nil, err
		}
		if tabula.ShapeEffects(result.effects.Bits)&tabula.EffectWrite == 0 || (len(result.fields) != 0) != returning {
			return nil, fmt.Errorf("tabula: write registration requires an actual write effect and matching result form")
		}
		if write, ok := node.(writeDeclaration); ok {
			for _, aliases := range write.aliases {
				for _, live := range family.live {
					if alias := aliases[live.input]; alias != nil {
						delete(aliases, live.input)
						live.input = alias
						family.live = append(family.live, live)
					}
				}
			}
		}
		if _, write := node.(writeDeclaration); write {
			result.shape = mergeQueryShapes(c, result.shape)
		}
		if returning {
			result.shape.ResultLineage = queryResultLineage(result.fields)
		}
		key := name + "." + scope + "." + form
		if len(filters) != 0 {
			key += ".where." + strings.Join(filters, "+")
		}
		c.hasSecret = c.hasSecret || family.meta.Secret
		shape, err := c.finishShape(key, result.sql, scopeType, result.shape)
		if err != nil {
			return nil, err
		}
		shape.compiled.WriteEffects = result.effects
		shape.compiled.FixedMethod = uint8(tabula.CallExec)
		if returning {
			shape.compiled.FixedMethod = uint8(tabula.CallReturningEach)
		}
		shape.shape.UnscopedOnly = shape.shape.UnscopedOnly || unscoped
		shape.compiled.UnscopedOnly = shape.shape.UnscopedOnly
		for _, live := range family.live {
			if position, used := shape.layout.positions[live.input]; used {
				shape.shape.Inputs[position].Secret = true
			}
		}
		shapes = append(shapes, shape)
		family.shapes[rowVariant{scope, strings.Join(filters, "+"), form}] = shape
	}
	if err := registerShapes(owner, name, shapes); err != nil {
		return nil, err
	}
	return family, nil
}
