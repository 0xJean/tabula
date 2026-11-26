package tabula

import "context"

// ExistsPlan owns an independent boolean probe on a physical row source.
// Each selected filter and scope has one registered statement.
type ExistsPlan[S any] struct {
	bind func(Session[S]) *BoundExistsPlan[S]
}

// DefineExists registers a SELECT EXISTS for the source's visible rows.
// It panics on a declaration error.
func DefineExists[T, S any](src RowSource[T, S], name string, filter RowFilter) *ExistsPlan[S] {
	p, err := TryDefineExists(src, name, filter)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineExists registers every allowed existence probe on the source
// table. A rejected declaration publishes no shapes.
func TryDefineExists[T, S any](src RowSource[T, S], name string, filter RowFilter) (*ExistsPlan[S], error) {
	owner := src.owner
	family, err := compileRowFamily(owner, name, []rowSource{src.rowSource}, filter, []string{"exists"},
		func(c *typedCompiler, _ string, predicates []Pred) (string, StatementShape, error) {
			return renderRowExists(c, src.rowSource, predicates)
		})
	if err != nil {
		return nil, err
	}
	return &ExistsPlan[S]{bind: func(s Session[S]) *BoundExistsPlan[S] {
		read := bindRowExecution(owner, family, s)
		return &BoundExistsPlan[S]{exists: read.exists}
	}}, nil
}

// BoundExistsPlan executes the existence probe on one authorized session.
type BoundExistsPlan[S any] struct {
	exists func(context.Context, []string, []Binding) (bool, error)
}

// Bind attaches the probe to a session issued for the source registry.
func (p *ExistsPlan[S]) Bind(s Session[S]) *BoundExistsPlan[S] {
	if p == nil || p.bind == nil {
		return &BoundExistsPlan[S]{}
	}
	return p.bind(s)
}

// Exists reports whether a visible row matches the selected filters.
// Supply only the bindings used by those filters.
func (b *BoundExistsPlan[S]) Exists(ctx context.Context, filters []string, values ...Binding) (bool, error) {
	if b == nil || b.exists == nil {
		return false, ErrStatementAuthority
	}
	return b.exists(ctx, filters, values)
}

func (b *rowExecution[T, S]) exists(ctx context.Context, filters []string, values []Binding) (bool, error) {
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
