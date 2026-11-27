package tabula

import (
	"context"
	"errors"
	"iter"
)

// BoundReturningPlan executes one registered write result using an operation
// savepoint. It requires OperationSession before any database I/O.
type BoundReturningPlan[T, S any] struct {
	rowExecution[T, S]
	plan *ReturningPlan[T, S]
}

// Bind attaches a returning plan to one authorized session.
func (p *ReturningPlan[T, S]) Bind(session Session[S]) *BoundReturningPlan[T, S] {
	if p == nil || p.owner == nil {
		return &BoundReturningPlan[T, S]{rowExecution: rowExecution[T, S]{readBinding: readBinding[T, S]{bindErr: ErrStatementAuthority}}}
	}
	execution := bindRowExecution(p.owner, p.family, session)
	execution.indexes = p.indexes
	return &BoundReturningPlan[T, S]{rowExecution: execution, plan: p}
}

func (b *BoundReturningPlan[T, S]) prepare(form string, filters []string, values []Binding) (writeChunk, error) {
	if b == nil || b.plan == nil {
		return writeChunk{}, ErrStatementAuthority
	}
	if b.bindErr != nil {
		return writeChunk{}, b.bindErr
	}
	shape, err := b.family.variant(b.s.Unscoped(), filters, form)
	if err != nil {
		return writeChunk{}, err
	}
	values = writeBindings(shape, values, b.family.live)
	if err := validateWriteBindings(b.d.reg, b.s, shape, CallReturningEach, values); err != nil {
		return writeChunk{}, err
	}
	return writeChunk{shape, values}, nil
}

// All decodes every returned row or returns no partial result on failure. The
// operation rolls back on database, cancellation or client decoding errors.
func (b *BoundReturningPlan[T, S]) All(ctx context.Context, values ...Binding) ([]T, error) {
	chunk, err := b.prepare("write", nil, values)
	if err != nil {
		return nil, err
	}
	return b.all(ctx, []writeChunk{chunk})
}

func (b *BoundReturningPlan[T, S]) all(ctx context.Context, chunks []writeChunk) ([]T, error) {
	var rows []T
	err := b.withResults(ctx, chunks, func(row T) bool {
		rows = append(rows, row)
		return true
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (b *BoundReturningPlan[T, S]) withResults(ctx context.Context, chunks []writeChunk, yield func(T) bool) error {
	if len(chunks) == 0 {
		return nil
	}
	operation, ok := b.s.(OperationSession[S])
	if !ok {
		return ErrOperationRequired
	}
	return operation.WithinOperation(ctx, func(ctx context.Context, session Session[S]) (err error) {
		stopped := false
		defer func() {
			if err == nil {
				err = ctx.Err()
			}
			if err != nil && stopped {
				// The consumer cannot receive another yield. Keep the diagnostic
				// visible through the enclosing transaction after operation rollback.
				err = errors.Join(ErrUnobservedWrite, err)
			}
		}()
		child := b.plan.Bind(session)
		if child.bindErr != nil {
			return child.bindErr
		}
		for _, chunk := range chunks {
			call, err := child.bindShape(chunk.shape, CallReturningEach, chunk.values)
			if err != nil {
				return err
			}
			dest := make([]any, child.resultWidth())
			_, err = child.run(ctx, "returning", call, func(ctx context.Context) (int64, error) {
				var count int64
				err := child.driver.EachAuthorized(ctx, call, func(scan func(...any) error) error {
					// A stop sentinel can hide a later server failure. Keep consuming
					// through command completion, without decoding or yielding again.
					if stopped {
						return nil
					}
					var row T
					if err := child.scanResult(scan, dest, &row); err != nil {
						return err
					}
					count++
					stopped = !yield(row)
					return nil
				})
				return count, err
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// Iter streams the one write result. Early break stops decoding and yielding,
// then drains the statement to observe completion or failure. It never narrows
// the affected rows. A failure discovered after break rolls back the operation
// and fails the enclosing transaction with ErrUnobservedWrite. A suspended pull
// iterator must be stopped before its enclosing runner ends; the session
// lifecycle closes and poisons leaked work.
func (b *BoundReturningPlan[T, S]) Iter(ctx context.Context, values ...Binding) iter.Seq2[T, error] {
	return b.iter(ctx, "write", nil, values)
}

func (b *BoundReturningPlan[T, S]) iter(ctx context.Context, form string, filters []string, values []Binding) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		chunk, err := b.prepare(form, filters, values)
		stopped := false
		if err == nil {
			err = b.withResults(ctx, []writeChunk{chunk}, func(row T) bool {
				stopped = !yield(row, nil)
				return !stopped
			})
		}
		if err != nil && !stopped {
			var zero T
			yield(zero, err)
		}
	}
}

// AllBatch returns every batch result with all-or-error rollback. Results have
// no promised order or positional association with input rows; conflicts and
// triggers can suppress rows. Project an explicit input key when needed.
func (b *BoundReturningPlan[T, S]) AllBatch(ctx context.Context, rows [][]Binding, values ...Binding) ([]T, error) {
	if b == nil || b.plan == nil {
		return nil, ErrStatementAuthority
	}
	if b.bindErr != nil {
		return nil, b.bindErr
	}
	if b.plan.batch == nil {
		return nil, ErrOperationDenied
	}
	chunks, err := b.plan.batch.prepare(b.family, b.s.Unscoped(), rows, values)
	if err != nil {
		return nil, err
	}
	for _, chunk := range chunks {
		if err := validateWriteBindings(b.d.reg, b.s, chunk.shape, CallReturningEach, chunk.values); err != nil {
			return nil, err
		}
	}
	return b.all(ctx, chunks)
}
