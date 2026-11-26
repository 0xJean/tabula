package tabula

import (
	"context"
	"fmt"
	"slices"
	"strconv"
)

// ReadSource identifies a physical occurrence in an explicit SELECT. FromTable
// and RowsFrom provide sources. Use RowsFrom to specify a scope or soft-delete
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

// GroupField is a physical field used in a SELECT grouping. Pass field.Expr()
// or field.Column(). Definitions check that every grouping belongs to the query.
type GroupField interface{ groupField() *fieldRef }

func (e Expr[V]) groupField() *fieldRef {
	f, _ := e.part.(*fieldRef)
	return f
}

func (c RowColumn) groupField() *fieldRef { return c.ref }

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
	unscoped      bool
	forms         ReadForms
}

// Select projects expressions into the declared output view in field order.
func Select(fields ...Projection) ReadSelect { return ReadSelect{project: slices.Clone(fields)} }

// From names the first physical occurrence.
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

// GroupBy groups rows by the declared fields of their respective occurrences.
func (q ReadSelect) GroupBy(fields ...GroupField) ReadSelect {
	q.group = slices.Concat(q.group, fields)
	return q
}

// Having filters aggregate groups after grouping.
func (q ReadSelect) Having(terms ...Pred) ReadSelect {
	q.having = slices.Concat(q.having, terms)
	return q
}

// OrderBy declares the ordered physical fields of this result.
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
	view   *View[T, S]
	family *rowFamily
	single singleRead
}

// DefineRead registers an explicit typed SELECT. It panics on a declaration error.
func DefineRead[T, S any](out *View[T, S], name string, query ReadSelect, observe ...StatementOption) *ReadPlan[T, S] {
	p, err := TryDefineRead(out, name, query, observe...)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineRead registers every permitted scope variant together. No shape is
// published when a declaration fails.
func TryDefineRead[T, S any](out *View[T, S], name string, query ReadSelect, observe ...StatementOption) (*ReadPlan[T, S], error) {
	if out == nil || isNilBound(query.from) {
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
	single, err := newSingleRead(query.forms, len(query.order) > 0, !query.lock.declared)
	if err != nil {
		return nil, err
	}
	forms := []string{"data"}
	if single.take != nil {
		forms = append(forms, single.form())
	}
	family, err := compileRowFamily(&out.descriptor, name, sources, RowFilter{AllowUnscoped: query.unscoped, Observe: observe}, forms,
		func(c *typedCompiler, form string, _ []Pred) (string, StatementShape, error) {
			var take *typedInput
			if form != "data" {
				take = single.take
			}
			return query.render(c, out.entry, sources, take)
		})
	if err != nil {
		return nil, err
	}
	return &ReadPlan[T, S]{out, family, single}, nil
}

// BoundReadPlan runs an explicit SELECT through the registry-authorized session.
type BoundReadPlan[T, S any] struct {
	rowExecution[T, S]
	single singleRead
}

// Bind attaches the plan to a session from its registry.
func (p *ReadPlan[T, S]) Bind(s Session[S]) *BoundReadPlan[T, S] {
	if p == nil || p.view == nil {
		var b BoundReadPlan[T, S]
		b.bindErr = ErrStatementAuthority
		return &b
	}
	return &BoundReadPlan[T, S]{bindRowExecution(&p.view.descriptor, p.family, s), p.single}
}

// Find decodes all matching result rows. Each caller input must be bound once.
func (b *BoundReadPlan[T, S]) Find(ctx context.Context, values ...Binding) ([]T, error) {
	if b == nil {
		return nil, ErrStatementAuthority
	}
	call, err := b.call(nil, "data", CallEach, values)
	if err != nil {
		return nil, err
	}
	return b.find(ctx, call)
}
