package plan

import (
	"context"
	"slices"

	"github.com/0xJean/tabula"
)

type rowExecution[T, S any] struct {
	d       *descriptor[T, S]
	s       tabula.Session[S]
	driver  tabula.AuthorizedSession
	bindErr error
	family  *rowFamily
	indexes []int
}

func bindRowExecution[T, S any](d *descriptor[T, S], f *rowFamily, s tabula.Session[S]) rowExecution[T, S] {
	b := rowExecution[T, S]{d: d, family: f}
	if d == nil || f == nil {
		b.bindErr = tabula.ErrStatementAuthority
		return b
	}
	if err := d.CheckSession(s); err != nil {
		b.bindErr = err
		return b
	}
	driver, ok := s.(tabula.AuthorizedSession)
	if !ok || !d.EligibleOwner(s) {
		b.bindErr = tabula.ErrStatementAuthority
		return b
	}
	b.s, b.driver = s, driver
	return b
}

func (b *rowExecution[T, S]) call(filters []string, form string, method tabula.CallMethod, bindings []Binding) (tabula.AuthorizedStatement, error) {
	if b.bindErr != nil {
		return tabula.AuthorizedStatement{}, b.bindErr
	}
	if b.family == nil {
		return tabula.AuthorizedStatement{}, tabula.ErrStatementAuthority
	}
	shape, err := b.family.variant(b.s.Unscoped(), filters, form)
	if err != nil {
		return tabula.AuthorizedStatement{}, err
	}
	bindings = append(slices.Clip(bindings), b.family.live...)
	return b.bindShape(shape, method, bindings)
}

func (b *rowExecution[T, S]) bindShape(shape *compiledShape, method tabula.CallMethod, values []Binding) (tabula.AuthorizedStatement, error) {
	return bindShapeCall(b.d, shape, b.s, method, values, b.family.meta.Quiet, false)
}

func (b *rowExecution[T, S]) run(ctx context.Context, action string, call tabula.AuthorizedStatement,
	execute func(context.Context) (int64, error),
) (int64, error) {
	return runOwnerOp(ctx, b.d, b.s, action, b.family.meta, call, execute)
}

// BoundRowsPlan executes registered statements on one authorized session.
type BoundRowsPlan[T, S any] struct {
	rowExecution[T, S]
	plan *RowsPlan[T, S]
}

// Bind attaches this read plan to a session from the same registry.
func (p *RowsPlan[T, S]) Bind(s tabula.Session[S]) *BoundRowsPlan[T, S] {
	if p == nil || p.owner == nil {
		var bound BoundRowsPlan[T, S]
		bound.bindErr = tabula.ErrStatementAuthority
		return &bound
	}
	exec := bindRowExecution(p.owner, p.family, s)
	exec.indexes = p.indexes
	return &BoundRowsPlan[T, S]{exec, p}
}

func (b *BoundRowsPlan[T, S]) checkRead() error {
	if b == nil || b.plan == nil {
		return tabula.ErrStatementAuthority
	}
	return b.bindErr
}

func (b *BoundRowsPlan[T, S]) checkSummary() error {
	if err := b.checkRead(); err != nil {
		return err
	}
	if b.plan.lock.declared {
		return tabula.ErrOperationDenied
	}
	return nil
}

func (b *BoundRowsPlan[T, S]) dataCall(choice RowChoice, bindings []Binding) (tabula.AuthorizedStatement, error) {
	if err := b.checkRead(); err != nil {
		return tabula.AuthorizedStatement{}, err
	}
	page := &b.plan.page
	if err := page.checkOrder(choice.Order); err != nil {
		return tabula.AuthorizedStatement{}, err
	}
	values, err := page.addPage(choice, slices.Clip(bindings))
	if err != nil {
		return tabula.AuthorizedStatement{}, err
	}
	return b.call(choice.Filters, dataForm(choice.Order, choice.Page), tabula.CallEach, values)
}

func (b *rowExecution[T, S]) resultWidth() int {
	if b.indexes == nil {
		return len(b.d.Columns)
	}
	return len(b.indexes)
}

func (b *rowExecution[T, S]) scanResult(scan func(...any) error, dst []any, row *T) error {
	if b.indexes == nil {
		return scan(b.d.FillAddrs(dst, row)...)
	}
	for i, index := range b.indexes {
		dst[i] = b.d.Columns[index].Target(row)
	}
	return scan(dst...)
}

func (b *rowExecution[T, S]) find(ctx context.Context, call tabula.AuthorizedStatement) ([]T, error) {
	var rows []T
	dst := make([]any, b.resultWidth())
	_, err := b.run(ctx, "find", call, func(ctx context.Context) (int64, error) {
		err := b.driver.EachAuthorized(ctx, call, func(scan func(...any) error) error {
			var row T
			if err := b.scanResult(scan, dst, &row); err != nil {
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

func (b *rowExecution[T, S]) count(ctx context.Context, call tabula.AuthorizedStatement) (int64, error) {
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

// Count counts unpaged result rows, including one row per group. Supply the
// bindings used by the selected predicates and group keys. A locking plan
// returns tabula.ErrOperationDenied.
func (b *BoundRowsPlan[T, S]) Count(ctx context.Context, filters []string, values ...Binding) (int64, error) {
	if err := b.checkSummary(); err != nil {
		return 0, err
	}
	call, err := b.call(filters, "count", tabula.CallScan, values)
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
		return false, tabula.ErrOperationDenied
	}
	return b.exists(ctx, filters, values)
}

// List returns a data page and the exact count before pagination. The two
// statements bind the inputs they need from the supplied values. Both are
// validated before the first execution. A locking plan returns tabula.ErrOperationDenied.
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
	for _, value := range values {
		if _, used := count.layout.positions[value.input]; used {
			countValues = append(countValues, value)
		}
	}
	countValues = append(countValues, b.family.live...)
	call, err := b.bindShape(count, tabula.CallScan, countValues)
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
func (p *MutationPlan[T, S]) Bind(s tabula.Session[S]) *BoundMutationPlan[T, S] {
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
		return 0, tabula.ErrOperationDenied
	}
	call, err := b.call(filters, form, tabula.CallExec, values)
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
