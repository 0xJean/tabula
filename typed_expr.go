package tabula

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Expr is a typed expression assembled from descriptor fields and inputs.
type Expr[V any] struct {
	part     expressionPart
	kind     PGType[V]
	nullable bool
}

// Pred is a PostgreSQL boolean expression. A nullable predicate follows SQL
// three-valued logic when it is used in WHERE.
type Pred = Expr[bool]

type expressionPart interface {
	render(*typedCompiler) (string, error)
}

type typedCompiler struct {
	sources    map[*occurrence]bool // true after rendering the occurrence's scope predicate
	from       []*occurrence
	params     *parameterLayout
	reads      map[physicalColumn]catalogType
	selectCols map[physicalName]map[string]bool
	hasSecret  bool
	scopeType  catalogType
}

func newTypedCompiler(scoped bool, sources ...*occurrence) (*typedCompiler, error) {
	c := &typedCompiler{
		sources: make(map[*occurrence]bool), from: slices.Clone(sources), reads: make(map[physicalColumn]catalogType),
		selectCols: make(map[physicalName]map[string]bool),
	}
	aliases := make(map[string]bool)
	needsScope := false
	for _, src := range sources {
		if src == nil || src.entry == nil || src.issuer == nil {
			return nil, fmt.Errorf("tabula: undeclared relation occurrence")
		}
		if src.issuer != sources[0].issuer {
			return nil, fmt.Errorf("tabula: relations belong to different registries")
		}
		if aliases[src.alias] {
			return nil, fmt.Errorf("tabula: duplicate relation alias %q", src.alias)
		}
		aliases[src.alias] = true
		c.sources[src] = false
		c.selectCols[src.table] = make(map[string]bool)
		needsScope = needsScope || src.scope == ByScopeColumn()
	}
	c.params = newParameterLayout(scoped && needsScope)
	return c, nil
}

// scopePredicate emits only the declared occurrence's scope condition. A
// database-enforced or global relation never reserves a session parameter.
func (c *typedCompiler) scopePredicate(src *occurrence, kind catalogType) (string, error) {
	if _, ok := c.sources[src]; !ok {
		return "", fmt.Errorf("tabula: undeclared scope occurrence")
	}
	if !c.params.scoped || src.scope != ByScopeColumn() {
		return "", nil
	}
	if kind.goType == nil || kind.goType != src.scopeGoType || (c.scopeType.goType != nil && !sameSQLType(c.scopeType, kind)) {
		return "", fmt.Errorf("tabula: incompatible PostgreSQL type for scope column on %q", src.alias)
	}
	col := physicalColumn{src.table, src.scopeColumn}
	if err := col.record(c.reads, kind); err != nil {
		return "", err
	}
	c.scopeType = kind
	c.selectCols[src.table][src.scopeColumn] = true
	c.sources[src] = true
	c.hasSecret = c.hasSecret || src.scopeSecret
	return "(" + typedQuote(src.alias) + "." + typedQuote(src.scopeColumn) + " = " + kind.cast("$1") + ")", nil
}

// readRequirements records each physical table once, even if two aliases
// refer to it. Ordered output makes the snapshot independent of map iteration.
func (c *typedCompiler) readRequirements() (StatementShape, error) {
	var s StatementShape
	s.Effects = EffectRead
	tables := make(map[string]physicalName)
	for _, src := range c.from {
		if err := src.checkCommand("SELECT"); err != nil {
			return StatementShape{}, err
		}
		tables[src.table.sql()] = src.table
	}
	for _, table := range slices.Sorted(maps.Keys(tables)) {
		s.Access = append(s.Access, ShapeAccess{table, "SELECT"})
		columns := slices.Sorted(maps.Keys(c.selectCols[tables[table]]))
		if len(columns) == 0 {
			s.AnyColumnSelect = append(s.AnyColumnSelect, table)
		} else {
			s.Privileges = append(s.Privileges, DefinedPrivilege{Table: table, Privilege: "SELECT", Columns: columns})
		}
	}
	return s, nil
}

// Expr references this column on its declaring occurrence.
func (f Field[V]) Expr() Expr[V] {
	if f.ref == nil {
		return Expr[V]{}
	}
	return Expr[V]{f.ref, f.kind, f.ref.nullable}
}

func (c *typedCompiler) read(f *fieldRef) error {
	if f == nil {
		return fmt.Errorf("tabula: undeclared field")
	}
	if _, ok := c.sources[f.source]; !ok {
		return fmt.Errorf("tabula: field is outside the statement's relation scope")
	}
	column := physicalColumn{f.source.table, f.name}
	if err := column.record(c.reads, f.kind); err != nil {
		return err
	}
	c.selectCols[f.source.table][f.name] = true
	c.hasSecret = c.hasSecret || f.secret
	return nil
}

func (f *fieldRef) render(c *typedCompiler) (string, error) {
	if err := c.read(f); err != nil {
		return "", err
	}
	return typedQuote(f.source.alias) + "." + typedQuote(f.name), nil
}

// Expr references a bound value. Its value never enters the SQL text.
func (s Slot[V]) Expr() Expr[V] {
	if s.input == nil {
		return Expr[V]{}
	}
	return Expr[V]{part: s.input, kind: s.kind, nullable: s.kind.nullable}
}

func (i *typedInput) render(c *typedCompiler) (string, error) {
	slot, err := c.params.parameter(i)
	if err != nil {
		return "", err
	}
	return i.kind.cast(slot), nil
}

func sameSQLType(a, b catalogType) bool {
	return a.goType != nil && b.goType != nil && a.schema == b.schema && a.name == b.name
}

type comparison struct {
	op           string
	left, right  expressionPart
	matchingType bool
}

func (v comparison) render(c *typedCompiler) (string, error) {
	if !v.matchingType || v.left == nil || v.right == nil {
		return "", fmt.Errorf("tabula: %s requires operands of the same declared PostgreSQL type", v.op)
	}
	left, err := v.left.render(c)
	if err != nil {
		return "", err
	}
	right, err := v.right.render(c)
	if err != nil {
		return "", err
	}
	return "(" + left + " " + v.op + " " + right + ")", nil
}

func typedComparison[V any](op string, left, right Expr[V], nullable bool) Pred {
	return Pred{comparison{op, left.part, right.part, sameSQLType(left.kind.id, right.kind.id)}, Bool(), nullable}
}

// Eq compares two expressions of the same PostgreSQL type.
func Eq[V any](left, right Expr[V]) Pred {
	return typedComparison("=", left, right, left.nullable || right.nullable)
}

// IsNotDistinct compares values using PostgreSQL's null-safe equality.
func IsNotDistinct[V any](left, right Expr[V]) Pred {
	return typedComparison("IS NOT DISTINCT FROM", left, right, false)
}

type conjunction []Pred

// And combines two or more typed predicates in declaration order.
func And(first, second Pred, rest ...Pred) Pred {
	terms := slices.Concat([]Pred{first, second}, rest)
	nullable := slices.ContainsFunc(terms, func(p Pred) bool { return p.nullable })
	return Pred{conjunction(terms), Bool(), nullable}
}

func (terms conjunction) render(c *typedCompiler) (string, error) {
	parts := make([]string, len(terms))
	for i, term := range terms {
		if term.part == nil || !sameSQLType(term.kind.id, Bool().id) {
			return "", fmt.Errorf("tabula: AND requires a declared boolean predicate")
		}
		part, err := term.part.render(c)
		if err != nil {
			return "", err
		}
		parts[i] = part
	}
	return "(" + strings.Join(parts, " AND ") + ")", nil
}
