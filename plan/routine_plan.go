package plan

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/0xJean/tabula"
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
func DefineRoutine[T, S, V any](view *tabula.View[T, S], key string, r *Routine[V], args ...RoutineArgument) *RoutinePlan[T, S] {
	p, err := TryDefineRoutine(view, key, r, args...)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineRoutine registers the complete call or returns a declaration error.
func TryDefineRoutine[T, S, V any](view *tabula.View[T, S], key string, r *Routine[V], args ...RoutineArgument) (*RoutinePlan[T, S], error) {
	owner := planView(view)
	if owner == nil || owner.Entry == nil || len(owner.Columns) != 1 || key == "" || r == nil || typeID(r.spec.Result).GoType == nil {
		return nil, errors.New("tabula: a registered one-column view, key and declared routine are required")
	}
	col := owner.Columns[0]
	result := typeID(r.spec.Result).GoType
	if !typedIdent(col.Name) || (col.GoType != result && (col.GoType.Kind() != reflect.Pointer || col.GoType.Elem() != result)) ||
		(r.spec.Nullable && !col.Nullable) {
		return nil, fmt.Errorf("tabula: incompatible result column %q for %s", col.Name, r.requirement.Signature)
	}
	c, err := newTypedCompiler(false)
	if err != nil {
		return nil, err
	}
	values, err := r.arguments(c, args)
	if err != nil {
		return nil, err
	}
	name := physicalName{Schema: r.spec.Schema, Name: r.spec.Name}
	call := name.SQL() + "(" + strings.Join(values, ", ") + ")"
	sql := "SELECT " + call + " AS " + typedQuote(col.Name)
	if r.spec.Cardinality == RoutineOne {
		sql = "SELECT (SELECT " + call + ") AS " + typedQuote(col.Name)
	}
	meta := tabula.StatementShape{
		Effects: tabula.EffectWrite, SchemaUsage: []string{r.spec.Schema},
		Routines:   []tabula.RoutineRequirement{r.requirement},
		Privileges: []tabula.DefinedPrivilege{{Function: r.requirement.Signature, Privilege: "EXECUTE"}},
		Result:     []tabula.ShapeField{{Name: col.Name, Type: r.requirement.ResultType, Nullable: r.spec.Nullable}},
	}
	if r.spec.ReadOnly {
		meta.Effects = tabula.EffectRead
	}
	for _, p := range r.params {
		meta.SchemaUsage = append(meta.SchemaUsage, p.kind.Schema)
	}
	c.hasSecret = c.hasSecret || col.Secret
	shape, err := c.finishShape(key+".call", sql, catalogType{}, meta)
	if err != nil {
		return nil, err
	}
	shape.compiled.WriteEffects = statementEffects{Bits: uint8(meta.Effects), WriteRoutine: !r.spec.ReadOnly}
	if err := registerShapes(owner, key, []*compiledShape{shape}); err != nil {
		return nil, err
	}
	return &RoutinePlan[T, S]{query: effectRows[T, S]{owner, shape}, set: r.requirement.ReturnsSet}, nil
}

// BoundRoutinePlan executes a registered function on one session.
type BoundRoutinePlan[T, S any] struct {
	plan    *RoutinePlan[T, S]
	session tabula.Session[S]
}

// Bind attaches the function plan to a session from its registry.
func (p *RoutinePlan[T, S]) Bind(s tabula.Session[S]) *BoundRoutinePlan[T, S] {
	return &BoundRoutinePlan[T, S]{plan: p, session: s}
}

// One decodes the single value of a RoutineOne call into the registered view.
func (b *BoundRoutinePlan[T, S]) One(ctx context.Context, args ...Binding) (T, error) {
	var row T
	if b == nil || b.plan == nil || b.plan.query.shape == nil {
		return row, tabula.ErrStatementAuthority
	}
	p := b.plan
	if p.set {
		return row, errors.New("tabula: One requires a scalar routine")
	}
	method := tabula.CallScan
	if p.query.shape.shape.Effects&tabula.EffectWrite != 0 {
		method = tabula.CallReturning
	}
	err := p.query.scan(ctx, b.session, method, args, func() []any {
		return p.query.owner.Addrs(&row)
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
		return nil, tabula.ErrStatementAuthority
	}
	p := b.plan
	if !p.set {
		return nil, errors.New("tabula: All requires a set-returning routine")
	}
	method := tabula.CallEach
	if p.query.shape.shape.Effects&tabula.EffectWrite != 0 {
		method = tabula.CallReturningEach
	}
	return p.query.all(ctx, b.session, method, args)
}

type effectRows[T, S any] struct {
	owner *descriptor[T, S]
	shape *compiledShape
}

func (q effectRows[T, S]) run(ctx context.Context, s tabula.Session[S], method tabula.CallMethod, bindings []Binding,
	execute func(context.Context, tabula.AuthorizedSession, tabula.AuthorizedStatement) (int64, error),
) error {
	call, err := bindShapeCall(q.owner, q.shape, s, method, bindings, false, false)
	if err != nil {
		return err
	}
	driver, ok := s.(tabula.AuthorizedSession)
	if !ok {
		return tabula.ErrStatementAuthority
	}
	_, err = runOwnerOp(ctx, q.owner, s, "", stmtMeta{}, call, func(ctx context.Context) (int64, error) {
		return execute(ctx, driver, call)
	})
	return err
}

func (q effectRows[T, S]) scan(ctx context.Context, s tabula.Session[S], method tabula.CallMethod, bindings []Binding, targets func() []any) error {
	return q.run(ctx, s, method, bindings, func(ctx context.Context, driver tabula.AuthorizedSession, call tabula.AuthorizedStatement) (int64, error) {
		if err := driver.ScanAuthorized(ctx, call, targets()...); err != nil {
			return 0, err
		}
		return 1, nil
	})
}

func (q effectRows[T, S]) all(ctx context.Context, s tabula.Session[S], method tabula.CallMethod, bindings []Binding) ([]T, error) {
	var rows []T
	err := q.run(ctx, s, method, bindings, func(ctx context.Context, driver tabula.AuthorizedSession, call tabula.AuthorizedStatement) (int64, error) {
		dst := make([]any, len(q.owner.Columns))
		err := driver.EachAuthorized(ctx, call, func(scan func(...any) error) error {
			var row T
			if err := scan(q.owner.FillAddrs(dst, &row)...); err != nil {
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
