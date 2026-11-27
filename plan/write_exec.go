package plan

import (
	"context"
	"slices"

	"github.com/0xJean/tabula"
)

// BoundWritePlan executes a registered write on one session.
type BoundWritePlan[S any] struct {
	plan    *WritePlan[S]
	s       tabula.Session[S]
	bindErr error
	run     func(context.Context, tabula.AuthorizedStatement) (int64, error)
}

func bindWritePlan[T, S any](plan *WritePlan[S], session tabula.Session[S], execution rowExecution[T, S]) *BoundWritePlan[S] {
	b := &BoundWritePlan[S]{plan: plan, s: session, bindErr: execution.bindErr}
	b.run = func(ctx context.Context, call tabula.AuthorizedStatement) (int64, error) {
		return execution.run(ctx, "write", call, func(ctx context.Context) (int64, error) {
			return execution.driver.ExecAuthorized(ctx, call)
		})
	}
	return b
}

// Bind attaches the write to a session from the declaring registry.
func (p *WritePlan[S]) Bind(session tabula.Session[S]) *BoundWritePlan[S] {
	if p == nil || p.bind == nil {
		return &BoundWritePlan[S]{bindErr: tabula.ErrStatementAuthority}
	}
	return p.bind(session)
}

// Exec executes the one declared statement and reports the outer command's
// affected count. A declared batch uses ExecBatch instead.
func (b *BoundWritePlan[S]) Exec(ctx context.Context, values ...Binding) (int64, error) {
	if b == nil || b.plan == nil {
		return 0, tabula.ErrStatementAuthority
	}
	if b.bindErr != nil {
		return 0, b.bindErr
	}
	if b.plan.batch != nil {
		return 0, tabula.ErrOperationDenied
	}
	shape, err := b.plan.family.variant(b.s.Unscoped(), nil, "write")
	if err != nil {
		return 0, err
	}
	return b.execute(ctx, shape, writeBindings(shape, values, b.plan.family.live))
}

func (b *BoundWritePlan[S]) execute(ctx context.Context, shape *compiledShape, values []Binding) (int64, error) {
	call, err := b.plan.bindCall(b.s, shape, tabula.CallExec, values)
	if err != nil {
		return 0, err
	}
	return b.run(ctx, call)
}

func writeBindings(shape *compiledShape, values, private []Binding) []Binding {
	out := slices.Clone(values)
	for _, value := range private {
		if _, used := shape.layout.positions[value.input]; used {
			out = append(out, value)
		}
	}
	return out
}

// Validate the original bindings before opening a savepoint. Driver adaptation
// occurs only when binding the actual call for the operation's sealed child.
func validateWriteBindings[T, S any](owner *descriptor[T, S], session tabula.Session[S], shape *compiledShape, method tabula.CallMethod, values []Binding) error {
	if owner == nil || shape == nil || shape.compiled == nil || shape.layout == nil {
		return tabula.ErrStatementAuthority
	}
	if err := owner.ValidateAuth(session, shape.compiled, uint8(method), shape.layout.scoped); err != nil {
		return err
	}
	_, err := shape.layout.bind(nil, values)
	return err
}

// ExecBatch validates every original row before I/O, chooses only declared
// row-count forms, and rolls all chunks back on any execution failure. Values
// supplies common inputs outside the VALUES template, such as conflict inputs.
// An empty batch performs no I/O and does not require OperationSession.
func (b *BoundWritePlan[S]) ExecBatch(ctx context.Context, rows [][]Binding, values ...Binding) (int64, error) {
	if b == nil || b.plan == nil {
		return 0, tabula.ErrStatementAuthority
	}
	if b.bindErr != nil {
		return 0, b.bindErr
	}
	if b.plan.batch == nil {
		return 0, tabula.ErrOperationDenied
	}
	chunks, err := b.plan.batch.prepare(b.plan.family, b.s.Unscoped(), rows, values)
	if err != nil {
		return 0, err
	}
	for _, chunk := range chunks {
		if err := b.plan.validate(b.s, chunk.shape, tabula.CallExec, chunk.values); err != nil {
			return 0, err
		}
	}
	if len(chunks) == 0 {
		return 0, nil
	}
	operation, ok := b.s.(tabula.OperationSession[S])
	if !ok {
		return 0, tabula.ErrOperationRequired
	}
	var total int64
	err = operation.WithinOperation(ctx, func(ctx context.Context, child tabula.Session[S]) error {
		bound := b.plan.Bind(child)
		if bound.bindErr != nil {
			return bound.bindErr
		}
		for _, chunk := range chunks {
			count, err := bound.execute(ctx, chunk.shape, chunk.values)
			if err != nil {
				return err
			}
			total += count
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}
