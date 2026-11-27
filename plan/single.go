package plan

import (
	"context"
	"errors"
	"slices"

	"github.com/0xJean/tabula"
)

type singleRead struct {
	ReadForms
	take      *typedInput
	unbounded bool
}

func newSingleRead(forms ReadForms, ordered, unbounded bool) (singleRead, error) {
	switch forms.First {
	case FirstDisabled:
	case OrderedFirst:
		if !ordered {
			return singleRead{}, errors.New("tabula: OrderedFirst requires a declared order")
		}
	case UnorderedFirst:
		if ordered {
			return singleRead{}, errors.New("tabula: UnorderedFirst requires an unordered read")
		}
	default:
		return singleRead{}, errors.New("tabula: unknown first-row mode")
	}
	s := singleRead{ReadForms: forms, unbounded: unbounded}
	if forms.Exact || forms.First != FirstDisabled {
		s.take = Input("@take", Int8()).input
	}
	return s, nil
}

// The bounded shape's name records which methods may use it in a snapshot.
func (s singleRead) form() string {
	switch {
	case s.Exact && s.First != FirstDisabled:
		return "single_first"
	case s.Exact:
		return "single"
	default:
		return "firstrow"
	}
}

func renderSingleLimit(c *typedCompiler, take *typedInput) (string, error) {
	if take == nil {
		return "", nil
	}
	slot, err := take.render(c)
	return " LIMIT " + slot, err
}

type singleMethod string

const (
	oneRow        singleMethod = "one"
	maybeOneRow   singleMethod = "maybe_one"
	firstRow      singleMethod = "first"
	maybeFirstRow singleMethod = "maybe_first"
)

func (b *rowExecution[T, S]) readSingle(ctx context.Context, policy singleRead, method singleMethod, choice RowChoice, values []Binding) (T, bool, error) {
	var zero T
	if b.bindErr != nil {
		return zero, false, b.bindErr
	}
	if choice.Page != Unpaged || choice.Limit != 0 || choice.Offset != 0 || choice.After.order != nil {
		return zero, false, errors.New("tabula: single-row reads do not accept pagination")
	}
	first := method == firstRow || method == maybeFirstRow
	if first && policy.First == FirstDisabled || !first && !policy.Exact && !policy.unbounded {
		return zero, false, tabula.ErrOperationDenied
	}
	bounded := first || policy.Exact
	form := dataForm(choice.Order, Unpaged)
	if bounded {
		form = orderedForm(policy.form(), choice.Order)
		limit := int64(2)
		if first {
			limit = 1
		}
		values = append(slices.Clip(values), Binding{input: policy.take, value: limit})
	}
	call, err := b.call(choice.Filters, form, tabula.CallEach, values)
	if err != nil {
		return zero, false, err
	}
	var row T
	var found bool
	_, err = b.run(ctx, string(method), call, func(ctx context.Context) (int64, error) {
		var seen int64
		err := b.driver.EachAuthorized(ctx, call, func(scan func(...any) error) error {
			seen++
			if seen == 1 {
				return b.scanResult(scan, make([]any, b.resultWidth()), &row)
			}
			if bounded {
				// PostgreSQL returns at most two rows. Finish iteration to
				// receive any final error before reporting a duplicate.
				return nil
			}
			return tabula.ErrMultipleRows
		})
		switch {
		case err != nil:
			return seen, err
		case seen > 1:
			return seen, tabula.ErrMultipleRows
		case seen == 0 && (method == oneRow || method == firstRow):
			return 0, tabula.ErrNotFound
		default:
			found = seen == 1
			return seen, nil
		}
	})
	if err != nil || !found {
		return zero, false, err
	}
	return row, true, nil
}

func (b *BoundRowsPlan[T, S]) singleRow(ctx context.Context, method singleMethod, choice RowChoice, values []Binding) (T, bool, error) {
	if err := b.checkRead(); err != nil {
		var zero T
		return zero, false, err
	}
	if err := b.plan.page.checkOrder(choice.Order); err != nil {
		var zero T
		return zero, false, err
	}
	return b.readSingle(ctx, b.plan.single, method, choice, values)
}

// One returns the only matching row. It returns tabula.ErrNotFound on an empty result
// and tabula.ErrMultipleRows on a second match. Declare Exact to limit PostgreSQL to
// two rows. Unlocked, unpaged reads can reuse their existing data statement.
func (b *BoundRowsPlan[T, S]) One(ctx context.Context, choice RowChoice, values ...Binding) (T, error) {
	row, _, err := b.singleRow(ctx, oneRow, choice, values)
	return row, err
}

// MaybeOne returns the only matching row and true. An empty result returns
// the zero T, false and nil. A second match returns tabula.ErrMultipleRows.
func (b *BoundRowsPlan[T, S]) MaybeOne(ctx context.Context, choice RowChoice, values ...Binding) (T, bool, error) {
	return b.singleRow(ctx, maybeOneRow, choice, values)
}

// First returns the first row under the declared first-row policy. PostgreSQL
// returns at most one row. An empty result returns tabula.ErrNotFound.
func (b *BoundRowsPlan[T, S]) First(ctx context.Context, choice RowChoice, values ...Binding) (T, error) {
	row, _, err := b.singleRow(ctx, firstRow, choice, values)
	return row, err
}

// MaybeFirst returns the first row and true under the declared first-row
// policy. An empty result returns the zero T, false and nil.
func (b *BoundRowsPlan[T, S]) MaybeFirst(ctx context.Context, choice RowChoice, values ...Binding) (T, bool, error) {
	return b.singleRow(ctx, maybeFirstRow, choice, values)
}

func (b *BoundReadPlan[T, S]) singleRow(ctx context.Context, method singleMethod, values []Binding) (T, bool, error) {
	if b == nil || b.family == nil {
		var zero T
		return zero, false, tabula.ErrStatementAuthority
	}
	return b.readSingle(ctx, b.single, method, RowChoice{}, values)
}

// One returns the only result row of the SELECT. It returns tabula.ErrNotFound on an
// empty result and tabula.ErrMultipleRows on a second match. An unlocked SELECT can
// reuse its data statement. WithForms can register a server limit of two.
func (b *BoundReadPlan[T, S]) One(ctx context.Context, values ...Binding) (T, error) {
	row, _, err := b.singleRow(ctx, oneRow, values)
	return row, err
}

// MaybeOne returns the only result row and true. An empty result returns the
// zero T, false and nil. A second match returns tabula.ErrMultipleRows.
func (b *BoundReadPlan[T, S]) MaybeOne(ctx context.Context, values ...Binding) (T, bool, error) {
	return b.singleRow(ctx, maybeOneRow, values)
}

// First returns the first result under the declared first-row policy, or
// tabula.ErrNotFound on an empty result. PostgreSQL returns at most one row.
func (b *BoundReadPlan[T, S]) First(ctx context.Context, values ...Binding) (T, error) {
	row, _, err := b.singleRow(ctx, firstRow, values)
	return row, err
}

// MaybeFirst returns the first result and true under the declared first-row
// policy. An empty result returns the zero T, false and nil.
func (b *BoundReadPlan[T, S]) MaybeFirst(ctx context.Context, values ...Binding) (T, bool, error) {
	return b.singleRow(ctx, maybeFirstRow, values)
}
