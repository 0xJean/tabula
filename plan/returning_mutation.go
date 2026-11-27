package plan

import (
	"github.com/0xJean/tabula"

	"context"
	"fmt"
	"iter"
	"slices"
)

// MutationReturningPlan owns a finite set/filter family and one result decoder.
// Each choice executes exactly one UPDATE or DELETE with RETURNING.
type MutationReturningPlan[T, S any] struct {
	plan    *ReturningPlan[T, S]
	command string
}

// DefineUpdateReturning registers finite assignment/filter choices with a
// result projection. Sets is a slice; projections are the sole variadic input.
func DefineUpdateReturning[R, T, S any](source RowSource[R, S], out *tabula.View[T, S], name string,
	spec MutationSpec, sets []RowSet, projections ...Projection,
) *MutationReturningPlan[T, S] {
	p, err := TryDefineUpdateReturning(source, out, name, spec, sets, projections...)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineUpdateReturning registers all UPDATE RETURNING choices atomically.
func TryDefineUpdateReturning[R, T, S any](source RowSource[R, S], out *tabula.View[T, S], name string,
	spec MutationSpec, sets []RowSet, projections ...Projection,
) (*MutationReturningPlan[T, S], error) {
	return compileMutationReturning(source, out, name, spec, sets, projections, "UPDATE")
}

// DefineDeleteReturning registers physical deletion with finite filter choices.
func DefineDeleteReturning[R, T, S any](source RowSource[R, S], out *tabula.View[T, S], name string,
	spec MutationSpec, projections ...Projection,
) *MutationReturningPlan[T, S] {
	p, err := TryDefineDeleteReturning(source, out, name, spec, projections...)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineDeleteReturning registers all DELETE RETURNING choices atomically.
func TryDefineDeleteReturning[R, T, S any](source RowSource[R, S], out *tabula.View[T, S], name string,
	spec MutationSpec, projections ...Projection,
) (*MutationReturningPlan[T, S], error) {
	return compileMutationReturning(source, out, name, spec, nil, projections, "DELETE")
}

func compileMutationReturning[R, T, S any](source RowSource[R, S], out *tabula.View[T, S], name string,
	spec MutationSpec, sets []RowSet, projections []Projection, command string,
) (*MutationReturningPlan[T, S], error) {
	owner := planView(out)
	if source.owner == nil || owner == nil || spec.Where.part == nil {
		return nil, fmt.Errorf("tabula: mutation RETURNING requires a source, output and fixed predicate")
	}
	base := newWriteDeclaration(source, command)
	base.where, base.version, base.returning = []Pred{spec.Where}, spec.Version, slices.Clone(projections)
	nodes := make(map[string]statementNode)
	if command == "DELETE" {
		nodes["delete"] = base
	} else {
		for _, set := range sets {
			if !rowLabel(set.Name) || len(set.Assignments) == 0 || nodes["set."+set.Name] != nil {
				return nil, fmt.Errorf("tabula: invalid or duplicate RETURNING assignment set")
			}
			n := base
			n.set = slices.Clone(set.Assignments)
			nodes["set."+set.Name] = n
		}
	}
	family, err := compileWriteFamily(owner, nil, name, nodes, false, spec.RowFilter, true)
	if err != nil {
		return nil, err
	}
	return &MutationReturningPlan[T, S]{plan: &ReturningPlan[T, S]{owner: owner, family: family}, command: command}, nil
}

// BoundMutationReturningPlan executes the permitted set/filter choices.
type BoundMutationReturningPlan[T, S any] struct {
	bound   *BoundReturningPlan[T, S]
	command string
}

// Bind attaches a finite mutation result family to one session.
func (p *MutationReturningPlan[T, S]) Bind(session tabula.Session[S]) *BoundMutationReturningPlan[T, S] {
	if p == nil {
		return &BoundMutationReturningPlan[T, S]{}
	}
	return &BoundMutationReturningPlan[T, S]{bound: p.plan.Bind(session), command: p.command}
}

// UpdateMany returns the new rows from the selected assignment/filter choice.
func (b *BoundMutationReturningPlan[T, S]) UpdateMany(ctx context.Context, set string, filters []string, values ...Binding) ([]T, error) {
	return b.all(ctx, "UPDATE", "set."+set, filters, values)
}

// DeleteMany returns the old rows deleted by the selected filter choice.
func (b *BoundMutationReturningPlan[T, S]) DeleteMany(ctx context.Context, filters []string, values ...Binding) ([]T, error) {
	return b.all(ctx, "DELETE", "delete", filters, values)
}

func (b *BoundMutationReturningPlan[T, S]) all(ctx context.Context, command, form string, filters []string, values []Binding) ([]T, error) {
	if b == nil || b.bound == nil || b.command != command {
		return nil, tabula.ErrOperationDenied
	}
	chunk, err := b.bound.prepare(form, filters, values)
	if err != nil {
		return nil, err
	}
	return b.bound.all(ctx, []writeChunk{chunk})
}

// UpdateIter streams new rows from the selected UPDATE with operation cleanup.
func (b *BoundMutationReturningPlan[T, S]) UpdateIter(ctx context.Context, set string, filters []string, values ...Binding) iter.Seq2[T, error] {
	return b.iter(ctx, "UPDATE", "set."+set, filters, values)
}

// DeleteIter streams old rows from the selected physical DELETE.
func (b *BoundMutationReturningPlan[T, S]) DeleteIter(ctx context.Context, filters []string, values ...Binding) iter.Seq2[T, error] {
	return b.iter(ctx, "DELETE", "delete", filters, values)
}

func (b *BoundMutationReturningPlan[T, S]) iter(ctx context.Context, command, form string, filters []string, values []Binding) iter.Seq2[T, error] {
	if b == nil || b.bound == nil || b.command != command {
		return func(yield func(T, error) bool) { var zero T; yield(zero, tabula.ErrOperationDenied) }
	}
	return b.bound.iter(ctx, form, filters, values)
}
