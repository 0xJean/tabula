package tabula

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// RoutinePlan executes one registered function call using its declared result
// cardinality and a one-column View's decoder.
type RoutinePlan[T, S any] struct {
	query effectRows[T, S]
	set   bool
}

// DefineRoutine registers a standalone call to a declared function. Each
// input needs exactly one Arg. Values bind in signature order. The output
// view must have one column compatible with the declared result.
func DefineRoutine[T, S, V any](view *View[T, S], key string, r *Routine[V], args ...RoutineArgument) *RoutinePlan[T, S] {
	p, err := TryDefineRoutine(view, key, r, args...)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineRoutine registers the complete call or returns a declaration error.
func TryDefineRoutine[T, S, V any](view *View[T, S], key string, r *Routine[V], args ...RoutineArgument) (*RoutinePlan[T, S], error) {
	if view == nil || view.entry == nil || len(view.cols) != 1 || key == "" || r == nil || r.spec.Result.id.goType == nil {
		return nil, errors.New("tabula: a registered one-column view, key and declared routine are required")
	}
	col := view.cols[0]
	result := r.spec.Result.id.goType
	if !typedIdent(col.name) || (col.goType != result && (col.goType.Kind() != reflect.Pointer || col.goType.Elem() != result)) ||
		(r.spec.Nullable && !col.nullable) {
		return nil, fmt.Errorf("tabula: incompatible result column %q for %s", col.name, r.requirement.Signature)
	}
	c, err := newTypedCompiler(false)
	if err != nil {
		return nil, err
	}
	values, err := r.arguments(c, args)
	if err != nil {
		return nil, err
	}
	name := physicalName{r.spec.Schema, r.spec.Name}
	call := name.sql() + "(" + strings.Join(values, ", ") + ")"
	sql := "SELECT " + call + " AS " + typedQuote(col.name)
	// A scalar subquery also rejects a catalog drift to a multi-row result
	// at execution, before a driver's single-row API could discard extra rows.
	if r.spec.Cardinality == RoutineOne {
		sql = "SELECT (SELECT " + call + ") AS " + typedQuote(col.name)
	}
	meta := StatementShape{
		Effects: EffectWrite, SchemaUsage: []string{r.spec.Schema},
		Routines:   []RoutineRequirement{r.requirement},
		Privileges: []DefinedPrivilege{{Function: r.requirement.Signature, Privilege: "EXECUTE"}},
		Result:     []ShapeField{{Name: col.name, Type: r.requirement.ResultType, Nullable: r.spec.Nullable}},
	}
	if r.spec.ReadOnly {
		meta.Effects = EffectRead
	}
	for _, p := range r.params {
		meta.SchemaUsage = append(meta.SchemaUsage, p.kind.schema)
	}
	c.hasSecret = c.hasSecret || col.secret
	shape, err := c.finishShape(key+".call", sql, catalogType{}, meta)
	if err != nil {
		return nil, err
	}
	shape.effects = statementEffects{bits: meta.Effects, writeRoutine: !r.spec.ReadOnly}
	if err := view.registerShapes(key, []*compiledShape{shape}); err != nil {
		return nil, err
	}
	return &RoutinePlan[T, S]{query: effectRows[T, S]{view, shape}, set: r.requirement.ReturnsSet}, nil
}

// BoundRoutinePlan executes a registered function on one session.
type BoundRoutinePlan[T, S any] struct {
	plan    *RoutinePlan[T, S]
	session Session[S]
}

// Bind attaches the function plan to a session from its registry.
func (p *RoutinePlan[T, S]) Bind(s Session[S]) *BoundRoutinePlan[T, S] {
	return &BoundRoutinePlan[T, S]{plan: p, session: s}
}

// One decodes the single value of a RoutineOne call into the registered view.
func (b *BoundRoutinePlan[T, S]) One(ctx context.Context, args ...Binding) (T, error) {
	var row T
	if b == nil || b.plan == nil || b.plan.query.shape == nil {
		return row, ErrStatementAuthority
	}
	p := b.plan
	if p.set {
		return row, errors.New("tabula: One requires a scalar routine")
	}
	method := CallScan
	if p.query.shape.shape.Effects&EffectWrite != 0 {
		method = CallReturning
	}
	err := p.query.scan(ctx, b.session, method, args, func() []any {
		decoder := readBinding[T, S]{d: &p.query.view.descriptor}
		return decoder.addrs(&row)
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return row, nil
}

// All decodes every value of a RoutineSet call, including an empty result.
func (b *BoundRoutinePlan[T, S]) All(ctx context.Context, args ...Binding) ([]T, error) {
	if b == nil || b.plan == nil || b.plan.query.shape == nil {
		return nil, ErrStatementAuthority
	}
	p := b.plan
	if !p.set {
		return nil, errors.New("tabula: All requires a set-returning routine")
	}
	method := CallEach
	if p.query.shape.shape.Effects&EffectWrite != 0 {
		method = CallReturningEach
	}
	return p.query.all(ctx, b.session, method, args)
}

// effectRows shares the authorized channel, registry observation and existing
// view decoder between routines and advisory locks. It never sends SQL text
// through a legacy Session method.
type effectRows[T, S any] struct {
	view  *View[T, S]
	shape *compiledShape
}

func (q effectRows[T, S]) run(ctx context.Context, s Session[S], method CallMethod, bindings []Binding,
	execute func(context.Context, AuthorizedSession, AuthorizedStatement) (int64, error),
) error {
	reg := q.view.reg
	call, err := reg.bindCall(q.shape, s, method, bindings)
	if err != nil {
		return err
	}
	driver, ok := s.(AuthorizedSession)
	if !ok {
		return ErrStatementAuthority
	}
	var observed ObserveMask
	if reporter, ok := s.(interface{ TabulaObserved() ObserveMask }); ok {
		observed = reporter.TabulaObserved()
	}
	ctx, span := reg.startOp(ctx, nil, s.Scope(), q.view.name, q.shape.action, stmtMeta{key: q.shape.observationKey}, call.secret, observed)
	n, err := execute(ctx, driver, call)
	span.finish(ctx, q.shape.sql, n, err, call.args, call.secret, false, false)
	if err != nil {
		return reg.classifyErr(ctx, err)
	}
	return nil
}

func (q effectRows[T, S]) scan(ctx context.Context, s Session[S], method CallMethod, bindings []Binding, targets func() []any) error {
	return q.run(ctx, s, method, bindings, func(ctx context.Context, driver AuthorizedSession, call AuthorizedStatement) (int64, error) {
		if err := driver.ScanAuthorized(ctx, call, targets()...); err != nil {
			return 0, err
		}
		return 1, nil
	})
}

func (q effectRows[T, S]) all(ctx context.Context, s Session[S], method CallMethod, bindings []Binding) ([]T, error) {
	var rows []T
	err := q.run(ctx, s, method, bindings, func(ctx context.Context, driver AuthorizedSession, call AuthorizedStatement) (int64, error) {
		decoder := readBinding[T, S]{d: &q.view.descriptor}
		dst := make([]any, len(q.view.cols))
		err := driver.EachAuthorized(ctx, call, func(scan func(...any) error) error {
			var row T
			if err := scan(decoder.fillAddrs(dst, &row)...); err != nil {
				return err
			}
			rows = append(rows, row)
			return nil
		})
		return int64(len(rows)), err
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}
