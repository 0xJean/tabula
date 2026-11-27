package plan

import (
	"github.com/0xJean/tabula"

	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ReadSource identifies a physical or query-result occurrence in an explicit
// SELECT. FromTable, RowsFrom and QuerySource provide sources. Use RowsFrom to
// specify a scope or soft-delete
// column with a custom PostgreSQL type.
type ReadSource interface {
	readSource() (rowSource, error)
}

func (r Rel[T]) readSource() (rowSource, error) {
	if r.rows == nil {
		return rowSource{}, fmt.Errorf("tabula: undeclared read source")
	}
	return r.rows()
}

func (r RowSource[T, S]) readSource() (rowSource, error) {
	if r.owner == nil || r.rel == nil {
		return rowSource{}, fmt.Errorf("tabula: undeclared row source")
	}
	return r.rowSource, nil
}

// GroupField is a sealed grouping expression. Pass field.Expr(), field.Column(),
// or a computed expression. Definitions validate its type and occurrence scope.
type GroupField interface{ groupOperand() scalarOperand }

func (e Expr[V]) groupOperand() scalarOperand { return e.scalarOperand() }
func (c RowColumn) groupOperand() scalarOperand {
	if c.ref != nil {
		return scalarOperand{c.ref, c.ref.kind, c.ref.nullable}
	}
	return c.expr
}

type readJoin struct {
	kind   string
	source ReadSource
	on     Pred
}

// ReadSelect is an immutable declaration of one explicit SELECT. Each method
// returns a new value. DefineRead freezes its SQL and verification metadata.
type ReadSelect struct {
	project       []Projection
	from          ReadSource
	joins         []readJoin
	where, having []Pred
	group         []GroupField
	order         []RowTerm
	lock          RowLock
	locks         []RowLock
	tables        []CommonTable
	set           *querySet
	limit         *queryLimit
	unscoped      bool
	forms         ReadForms
	distinct      bool
	distinctOn    []scalarOperand
	clauseErr     error
	list          *ReadListSpec
	listRepeated  bool
}

// Select projects expressions into a final output view or intermediate exports.
// Without From, it selects self-contained expressions and subqueries.
func Select(fields ...Projection) ReadSelect { return ReadSelect{project: slices.Clone(fields)} }

// From names the first physical or query-result occurrence.
func (q ReadSelect) From(source ReadSource) ReadSelect {
	q.from = source
	return q
}

// InnerJoin adds matching rows from an explicitly named right occurrence.
// The predicate can reference the right occurrence and preceding sources.
func (q ReadSelect) InnerJoin(right ReadSource, on Pred) ReadSelect {
	q.joins = append(slices.Clip(q.joins), readJoin{"INNER JOIN", right, on})
	return q
}

// LeftJoin preserves left rows when the right occurrence has no visible
// match. Its scope and live-row conditions are part of ON.
func (q ReadSelect) LeftJoin(right ReadSource, on Pred) ReadSelect {
	q.joins = append(slices.Clip(q.joins), readJoin{"LEFT JOIN", right, on})
	return q
}

// CrossJoin returns the product of the visible rows of both sides.
func (q ReadSelect) CrossJoin(right ReadSource) ReadSelect {
	q.joins = append(slices.Clip(q.joins), readJoin{kind: "CROSS JOIN", source: right})
	return q
}

// Where restricts rows before grouping. Multiple terms are combined with AND.
func (q ReadSelect) Where(terms ...Pred) ReadSelect {
	q.where = slices.Concat(q.where, terms)
	return q
}

// GroupBy groups rows by declared typed expressions.
func (q ReadSelect) GroupBy(fields ...GroupField) ReadSelect {
	q.group = slices.Concat(q.group, fields)
	return q
}

// Having filters aggregate groups after grouping.
func (q ReadSelect) Having(terms ...Pred) ReadSelect {
	q.having = slices.Concat(q.having, terms)
	return q
}

// OrderBy declares the typed ordering expressions of this result.
func (q ReadSelect) OrderBy(terms ...RowTerm) ReadSelect {
	q.order = slices.Concat(q.order, terms)
	return q
}

// WithForms selects the permitted bounded reads. OrderedFirst requires
// OrderBy and UnorderedFirst requires an unordered SELECT.
func (q ReadSelect) WithForms(forms ReadForms) ReadSelect {
	q.forms = forms
	return q
}

// Lock applies a row lock to one eligible occurrence of an ungrouped read.
func (q ReadSelect) Lock(lock RowLock) ReadSelect {
	q.lock = lock
	q.locks = nil
	return q
}

// AllowUnscoped also registers the form that omits column-scope predicates.
// It requires an unscoped session. PostgreSQL RLS applies in both forms.
func (q ReadSelect) AllowUnscoped() ReadSelect {
	q.unscoped = true
	return q
}

// ReadPlan owns the compiled scoped forms of an explicit SELECT.
type ReadPlan[T, S any] struct {
	owner   *descriptor[T, S]
	indexes []int
	family  *rowFamily
	single  singleRead
	list    *readListPlan
}

// DefineRead registers an explicit typed SELECT. It panics on a declaration error.
func DefineRead[T, S any](out *tabula.View[T, S], name string, query ReadSelect, observe ...tabula.StatementOption) *ReadPlan[T, S] {
	p, err := TryDefineRead(out, name, query, observe...)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineRead registers every permitted scope variant together. No shape is
// published when a declaration fails.
func TryDefineRead[T, S any](out *tabula.View[T, S], name string, query ReadSelect, observe ...tabula.StatementOption) (*ReadPlan[T, S], error) {
	if out == nil {
		return nil, fmt.Errorf("tabula: DefineRead requires an output view")
	}
	return compileReadPlan(planView(out), nil, name, query, observe)
}

func compileReadPlan[T, S any](owner *descriptor[T, S], indexes []int, name string, query ReadSelect, observe []tabula.StatementOption) (*ReadPlan[T, S], error) {
	if query.composed() {
		return compileQueryPlan(owner, indexes, name, query, observe)
	}
	if owner == nil || isNilBound(query.from) {
		return nil, fmt.Errorf("tabula: DefineRead requires a view and a FROM occurrence")
	}
	first, err := query.from.readSource()
	if err != nil {
		return nil, err
	}
	sources := []rowSource{first}
	for i, join := range query.joins {
		if isNilBound(join.source) {
			return nil, fmt.Errorf("tabula: join %d needs a right occurrence", i+1)
		}
		right, err := join.source.readSource()
		if err != nil {
			return nil, err
		}
		if live := right.liveValue.input; live != nil {
			// Every occurrence can carry its own private soft-delete value.
			right.liveValue.input = &typedInput{name: "@live." + strconv.Itoa(i+1), kind: live.kind}
		}
		sources = append(sources, right)
	}
	single, err := newSingleRead(query.forms, len(query.order) > 0, !query.hasRowLocks())
	if err != nil {
		return nil, err
	}
	forms := []string{"data"}
	if single.take != nil {
		forms = append(forms, single.form())
	}
	list, err := compileReadList(query, sources)
	if err != nil {
		return nil, err
	}
	spec := RowFilter{AllowUnscoped: query.unscoped, Observe: observe}
	var reachable func([]shapeAlternative) bool
	if list != nil {
		spec.Optional, spec.Budget = query.list.Optional, query.list.Budget
		forms = append(forms, list.forms...)
		reachable = fixedReadChoices
	}
	family, err := compileRowFamilyChoices(owner, name, sources, spec, forms, reachable,
		func(c *typedCompiler, form string, predicates []Pred) (string, tabula.StatementShape, error) {
			if strings.HasPrefix(form, "list.") {
				return list.render(c, ownerOutputLayout(owner), indexes, query.Where(predicates...), sources, form)
			}
			var take *typedInput
			if form != "data" {
				take = single.take
			}
			text, meta, err := query.render(c, ownerOutputLayout(owner), indexes, sources, take)
			if err == nil && len(query.distinctOn) != 0 {
				meta.Keys, meta.Comparisons, err = readWinnerProof(query, sources)
			}
			return text, meta, err
		})
	if err != nil {
		return nil, err
	}
	return &ReadPlan[T, S]{owner: owner, indexes: slices.Clone(indexes), family: family, single: single, list: list}, nil
}

// BoundReadPlan runs an explicit SELECT through the registry-authorized session.
type BoundReadPlan[T, S any] struct {
	rowExecution[T, S]
	single singleRead
	list   *readListPlan
}

// Bind attaches the plan to a session from its registry.
func (p *ReadPlan[T, S]) Bind(s tabula.Session[S]) *BoundReadPlan[T, S] {
	if p == nil || p.owner == nil {
		var b BoundReadPlan[T, S]
		b.bindErr = tabula.ErrStatementAuthority
		return &b
	}
	execution := bindRowExecution(p.owner, p.family, s)
	execution.indexes = p.indexes
	return &BoundReadPlan[T, S]{execution, p.single, p.list}
}

// Find decodes all matching result rows. Each caller input must be bound once.
func (b *BoundReadPlan[T, S]) Find(ctx context.Context, values ...Binding) ([]T, error) {
	if b == nil {
		return nil, tabula.ErrStatementAuthority
	}
	call, err := b.call(nil, "data", tabula.CallEach, values)
	if err != nil {
		return nil, err
	}
	return b.find(ctx, call)
}
