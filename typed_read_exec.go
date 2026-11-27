package tabula

import (
	"context"
	"fmt"
	"slices"
)

// Cursor checks manually encoded projected order values against this plan.
// Include every term in declaration order, including nullable join keys.
func (p *ReadPlan[T, S]) Cursor(order string, terms ...CursorValue) (RowCursor, error) {
	if p == nil || p.list == nil || !p.list.page.pages.Keyset || p.list.page.orders[order] == nil {
		return RowCursor{}, fmt.Errorf("tabula: unknown keyset order")
	}
	return cursorForOrder(p.list.page.orders[order], terms)
}

// CursorFrom obtains the boundary from the decoded projected values of row.
// It uses the compiled descriptor indexes, including partial table selections.
func (p *ReadPlan[T, S]) CursorFrom(order string, row T) (RowCursor, error) {
	if p == nil || p.list == nil || !p.list.page.pages.Keyset || p.list.page.orders[order] == nil {
		return RowCursor{}, fmt.Errorf("tabula: unknown keyset order")
	}
	return cursorFromRow(p.owner, p.list.page.orders[order], row)
}

func (b *BoundReadPlan[T, S]) checkList(summary bool) error {
	if b == nil {
		return ErrStatementAuthority
	}
	if b.bindErr != nil {
		return b.bindErr
	}
	if b.list == nil || summary && !b.list.exact {
		return ErrOperationDenied
	}
	return nil
}

func (b *BoundReadPlan[T, S]) pageShape(choice RowChoice, values []Binding) (*compiledShape, []Binding, error) {
	if err := b.checkList(false); err != nil {
		return nil, nil, err
	}
	if err := b.list.page.checkOrder(choice.Order); err != nil {
		return nil, nil, err
	}
	shape, err := b.family.variant(b.s.Unscoped(), choice.Filters, "list."+dataForm(choice.Order, choice.Page))
	if err != nil {
		return nil, nil, err
	}
	values, err = b.list.page.addPage(choice, slices.Clip(values))
	return shape, values, err
}

// Page executes one registered list data shape. Find retains the fixed read.
func (b *BoundReadPlan[T, S]) Page(ctx context.Context, choice RowChoice, values ...Binding) ([]T, error) {
	shape, values, err := b.pageShape(choice, values)
	if err != nil {
		return nil, err
	}
	call, err := b.bindShape(shape, CallEach, append(values, b.family.live...))
	if err != nil {
		return nil, err
	}
	return b.find(ctx, call)
}

// Count returns the exact filtered unpaged result cardinality. Bind only
// inputs retained by the summary. It requires WithList with ExactCount.
func (b *BoundReadPlan[T, S]) Count(ctx context.Context, filters []string, values ...Binding) (int64, error) {
	if err := b.checkList(true); err != nil {
		return 0, err
	}
	call, err := b.call(filters, "list.count", CallScan, values)
	if err != nil {
		return 0, err
	}
	return b.count(ctx, call)
}

// List returns a page and exact unpaged cardinality, or no partial result on
// failure. Both shapes validate before I/O and independently adapt the original
// bindings. Choose a transaction snapshot when both reads must see one state.
func (b *BoundReadPlan[T, S]) List(ctx context.Context, choice RowChoice, values ...Binding) ([]T, int64, error) {
	if err := b.checkList(true); err != nil {
		return nil, 0, err
	}
	data, values, err := b.pageShape(choice, values)
	if err != nil {
		return nil, 0, err
	}
	count, err := b.family.variant(b.s.Unscoped(), choice.Filters, "list.count")
	if err != nil {
		return nil, 0, err
	}
	values = append(values, b.family.live...)
	bindings, err := readPairBindings(values, data, count)
	if err != nil {
		return nil, 0, err
	}
	dataCall, err := b.bindShape(data, CallEach, bindings[0])
	if err != nil {
		return nil, 0, err
	}
	countCall, err := b.bindShape(count, CallScan, bindings[1])
	if err != nil {
		return nil, 0, err
	}
	total, err := b.count(ctx, countCall)
	if err != nil {
		return nil, 0, err
	}
	rows, err := b.find(ctx, dataCall)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// Check original identities and the union before selecting either dense layout.
// Reused values stay canonical; neither driver's adapted arguments are reused.
func readPairBindings(values []Binding, shapes ...*compiledShape) ([][]Binding, error) {
	required := make(map[*typedInput]bool)
	for _, shape := range shapes {
		for _, input := range shape.layout.order {
			if input.fixed == nil {
				required[input] = false
			}
		}
	}
	for _, value := range values {
		seen, ok := required[value.input]
		if !ok {
			return nil, fmt.Errorf("tabula: binding does not belong to this list")
		}
		if seen {
			return nil, fmt.Errorf("tabula: duplicate binding for %q", value.input.name)
		}
		if value.err != nil {
			return nil, fmt.Errorf("tabula: encode input %q: %w", value.input.name, value.err)
		}
		required[value.input] = true
	}
	result := make([][]Binding, len(shapes))
	for i, shape := range shapes {
		for _, input := range shape.layout.order {
			// Private Take values are filled by each layout's bind operation.
			if input.fixed == nil && !required[input] {
				return nil, fmt.Errorf("tabula: missing binding for %q", input.name)
			}
		}
		for _, value := range values {
			if _, ok := shape.layout.positions[value.input]; ok {
				result[i] = append(result[i], value)
			}
		}
	}
	return result, nil
}
