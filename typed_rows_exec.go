package tabula

import (
	"context"
	"fmt"
	"slices"
)

type rowExecution[T, S any] struct {
	readBinding[T, S]
	driver AuthorizedSession
	family *rowFamily
}

func bindRowExecution[T, S any](d *descriptor[T, S], f *rowFamily, s Session[S]) rowExecution[T, S] {
	b := rowExecution[T, S]{family: f}
	if d == nil || f == nil {
		b.bindErr = ErrStatementAuthority
		return b
	}
	if err := d.reg.checkSession(s); err != nil {
		b.bindErr = err
		return b
	}
	driver, ok := s.(AuthorizedSession)
	if !ok || !eligibleOwner(s) {
		b.bindErr = ErrStatementAuthority
		return b
	}
	b.readBinding, b.driver = bindReader(d, s), driver
	return b
}

func (b *rowExecution[T, S]) call(filters []string, form string, method CallMethod, bindings []Binding) (AuthorizedStatement, error) {
	if b.bindErr != nil {
		return AuthorizedStatement{}, b.bindErr
	}
	if b.family == nil {
		return AuthorizedStatement{}, ErrStatementAuthority
	}
	shape, err := b.family.variant(b.s.Unscoped(), filters, form)
	if err != nil {
		return AuthorizedStatement{}, err
	}
	bindings = append(slices.Clip(bindings), b.family.live...)
	return b.bindShape(shape, method, bindings)
}

func (b *rowExecution[T, S]) bindShape(shape *compiledShape, method CallMethod, values []Binding) (AuthorizedStatement, error) {
	call, err := b.d.reg.bindCall(shape, b.s, method, values)
	if err == nil {
		call.quiet = b.family.meta.quiet
	}
	return call, err
}

func (b *rowExecution[T, S]) run(ctx context.Context, action string, call AuthorizedStatement,
	execute func(context.Context) (int64, error),
) (int64, error) {
	m := b.family.meta
	m.key = call.shape.observationKey
	secret := make([]bool, len(call.shape.shape.Inputs))
	for i, in := range call.shape.shape.Inputs {
		secret[i] = in.Secret
	}
	ctx, span := b.start(ctx, action, m, secret)
	n, raw := execute(ctx)
	span.finish(ctx, call.shape.sql, n, raw, call.args, secret, m.secret, m.quiet)
	return n, b.fail(ctx, raw)
}

// BoundRowsPlan executes registered statements on one authorized session.
type BoundRowsPlan[T, S any] struct {
	rowExecution[T, S]
	plan *RowsPlan[T, S]
}

// Bind attaches this read plan to a session from the same registry.
func (p *RowsPlan[T, S]) Bind(s Session[S]) *BoundRowsPlan[T, S] {
	if p == nil || p.view == nil {
		var bound BoundRowsPlan[T, S]
		bound.bindErr = ErrStatementAuthority
		return &bound
	}
	return &BoundRowsPlan[T, S]{bindRowExecution(&p.view.descriptor, p.family, s), p}
}

func (b *BoundRowsPlan[T, S]) checkRead() error {
	if b == nil || b.plan == nil {
		return ErrStatementAuthority
	}
	return b.bindErr
}

func (b *BoundRowsPlan[T, S]) checkSummary() error {
	if err := b.checkRead(); err != nil {
		return err
	}
	if b.plan.lock.declared {
		return ErrOperationDenied
	}
	return nil
}

func (b *BoundRowsPlan[T, S]) dataCall(choice RowChoice, bindings []Binding) (AuthorizedStatement, error) {
	if err := b.checkRead(); err != nil {
		return AuthorizedStatement{}, err
	}
	page := &b.plan.page
	if (choice.Order == "") != (len(page.orders) == 0) || choice.Order != "" && page.orders[choice.Order] == nil {
		return AuthorizedStatement{}, fmt.Errorf("tabula: unregistered row order %q", choice.Order)
	}
	values, err := page.addPage(choice, slices.Clip(bindings))
	if err != nil {
		return AuthorizedStatement{}, err
	}
	return b.call(choice.Filters, dataForm(choice.Order, choice.Page), CallEach, values)
}

func (b *rowExecution[T, S]) find(ctx context.Context, call AuthorizedStatement) ([]T, error) {
	var rows []T
	dst := make([]any, len(b.d.cols))
	_, err := b.run(ctx, "find", call, func(ctx context.Context) (int64, error) {
		err := b.driver.EachAuthorized(ctx, call, func(scan func(...any) error) error {
			var row T
			if err := scan(b.fillAddrs(dst, &row)...); err != nil {
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

func (b *BoundRowsPlan[T, S]) count(ctx context.Context, call AuthorizedStatement) (int64, error) {
	var total int64
	_, err := b.run(ctx, "count", call, func(ctx context.Context) (int64, error) {
		return -1, b.driver.ScanAuthorized(ctx, call, &total)
	})
	return total, err
}

// Find returns the selected data page. It executes one registered statement.
func (b *BoundRowsPlan[T, S]) Find(ctx context.Context, choice RowChoice, values ...Binding) ([]T, error) {
	call, err := b.dataCall(choice, values)
	if err != nil {
		return nil, err
	}
	return b.find(ctx, call)
}

// Count counts unpaged result rows (groups for grouped queries). Supply only
// the bindings used by the selected WHERE predicates. A locking plan returns
// ErrOperationDenied.
func (b *BoundRowsPlan[T, S]) Count(ctx context.Context, filters []string, values ...Binding) (int64, error) {
	if err := b.checkSummary(); err != nil {
		return 0, err
	}
	call, err := b.call(filters, "count", CallScan, values)
	if err != nil {
		return 0, err
	}
	return b.count(ctx, call)
}

// Exists reports whether a visible row satisfies the selected filter.
// Declare WithExists to enable it. It ignores data-page ordering and bounds.
func (b *BoundRowsPlan[T, S]) Exists(ctx context.Context, filters []string, values ...Binding) (bool, error) {
	if err := b.checkSummary(); err != nil {
		return false, err
	}
	if !b.plan.withExists {
		return false, ErrOperationDenied
	}
	call, err := b.call(filters, "exists", CallScan, values)
	if err != nil {
		return false, err
	}
	var found bool
	_, err = b.run(ctx, "exists", call, func(ctx context.Context) (int64, error) {
		return -1, b.driver.ScanAuthorized(ctx, call, &found)
	})
	return found && err == nil, err
}

// List returns a data page and the exact count before pagination. Both
// registered calls and all bindings are validated before the first execution.
// A locking plan returns ErrOperationDenied.
func (b *BoundRowsPlan[T, S]) List(ctx context.Context, choice RowChoice, values ...Binding) ([]T, int64, error) {
	if err := b.checkSummary(); err != nil {
		return nil, 0, err
	}
	data, err := b.dataCall(choice, values)
	if err != nil {
		return nil, 0, err
	}
	count, err := b.family.variant(b.s.Unscoped(), choice.Filters, "count")
	if err != nil {
		return nil, 0, err
	}
	countValues := make([]Binding, 0, len(count.layout.order))
	for _, in := range count.layout.order {
		position, ok := data.shape.layout.positions[in]
		if !ok {
			return nil, 0, fmt.Errorf("tabula: count input %q missing from data shape", in.name)
		}
		countValues = append(countValues, Binding{in, data.args[position]})
	}
	call, err := b.bindShape(count, CallScan, countValues)
	if err != nil {
		return nil, 0, err
	}
	total, err := b.count(ctx, call)
	if err != nil {
		return nil, 0, err
	}
	rows, err := b.find(ctx, data)
	return rows, total, err
}

// BoundMutationPlan executes one declared mutation family on an authorized session.
type BoundMutationPlan[T, S any] struct {
	rowExecution[T, S]
	command string
}

// Bind attaches this mutation plan to one session.
func (p *MutationPlan[T, S]) Bind(s Session[S]) *BoundMutationPlan[T, S] {
	return &BoundMutationPlan[T, S]{bindRowExecution(p.source.owner, p.family, s), p.command}
}

// UpdateMany selects a complete named assignment set and returns the exact
// affected row count, including zero. The filter list selects optional predicates.
func (b *BoundMutationPlan[T, S]) UpdateMany(ctx context.Context, set string, filters []string, values ...Binding) (int64, error) {
	return b.mutate(ctx, "UPDATE", "set."+set, filters, values)
}

// DeleteMany physically deletes matching live rows and returns the exact affected count.
func (b *BoundMutationPlan[T, S]) DeleteMany(ctx context.Context, filters []string, values ...Binding) (int64, error) {
	return b.mutate(ctx, "DELETE", "delete", filters, values)
}

func (b *BoundMutationPlan[T, S]) mutate(ctx context.Context, command, form string, filters []string, values []Binding) (int64, error) {
	if b.command != command {
		return 0, ErrOperationDenied
	}
	call, err := b.call(filters, form, CallExec, values)
	if err != nil {
		return 0, err
	}
	action := "update_many"
	if command == "DELETE" {
		action = "delete_many"
	}
	return b.run(ctx, action, call, func(ctx context.Context) (int64, error) {
		return b.driver.ExecAuthorized(ctx, call)
	})
}
