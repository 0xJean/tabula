package tabula

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// SQLQuery is a compiled, registered read-only named-parameter SQL query
// scanning results into Row and binding parameters from Args.
type SQLQuery[Row, Args, S any] struct {
	plan *compiledSQLPlan[Args, S]
	row  *descriptor[Row, S]
}

// DefineSQL validates and registers a read-only named-parameter SQL query on r.
// It accepts SELECT, WITH ... SELECT, VALUES, and row-locking SELECT ... FOR
// UPDATE/SHARE (automatically marking EffectRowLock), and rejects writes and
// modifying CTEs. Panics on declaration mistakes at initialization.
func DefineSQL[Row, Args, S any](r *Registry[S], spec SQLSpec) *SQLQuery[Row, Args, S] {
	q, err := TryDefineSQL[Row, Args](r, spec)
	if err != nil {
		panic(err)
	}
	return q
}

// TryDefineSQL validates and atomically registers a read-only named-parameter
// SQL query on r, publishing nothing on error.
func TryDefineSQL[Row, Args, S any](r *Registry[S], spec SQLSpec) (*SQLQuery[Row, Args, S], error) {
	if r == nil {
		return nil, errors.New("tabula.DefineSQL: registry is required")
	}
	name := strings.TrimSpace(spec.Name)
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s.DefineSQL %s: "+format, append([]any{r.prefix, name}, args...)...)
	}
	rowDesc, resultFields, err := compileSQLRow[Row](r, name, fail)
	if err != nil {
		return nil, err
	}
	plan, err := compileSQLPlan[Args](r, spec, sqlKindRead, "DefineSQL", resultFields, rowDesc.definedColumns())
	if err != nil {
		return nil, err
	}
	return &SQLQuery[Row, Args, S]{plan: plan, row: rowDesc}, nil
}

// Name returns the registered declaration name.
func (q *SQLQuery[Row, Args, S]) Name() string { return q.plan.name }

// SQL returns the compiled positional PostgreSQL SQL ($1..$n).
func (q *SQLQuery[Row, Args, S]) SQL() string { return q.plan.shape.SQL }

// Definition returns a deep copy of the registered Definition for sqlcheck.
func (q *SQLQuery[Row, Args, S]) Definition() Definition { return q.plan.definition() }

// Bind attaches this query to an authorized session from the owning Registry.
func (q *SQLQuery[Row, Args, S]) Bind(s Session[S]) *BoundSQLQuery[Row, Args, S] {
	if q == nil || q.plan == nil {
		return &BoundSQLQuery[Row, Args, S]{binding: sqlSessionBinding[S]{bindErr: ErrStatementAuthority}}
	}
	return &BoundSQLQuery[Row, Args, S]{q: q, binding: bindSQLSession(q.plan.reg, s)}
}

// BoundSQLQuery executes a registered SQLQuery on one authorized session.
type BoundSQLQuery[Row, Args, S any] struct {
	q       *SQLQuery[Row, Args, S]
	binding sqlSessionBinding[S]
}

// WithLogger returns a handle with a per-binding logger override.
func (b *BoundSQLQuery[Row, Args, S]) WithLogger(logger *slog.Logger) *BoundSQLQuery[Row, Args, S] {
	clone := *b
	clone.binding.logger = logger
	return &clone
}

func (b *BoundSQLQuery[Row, Args, S]) prepare(args *Args) (AuthorizedStatement, error) {
	if b == nil || b.q == nil || b.q.plan == nil {
		return AuthorizedStatement{}, ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return AuthorizedStatement{}, b.binding.bindErr
	}
	values, secret, err := b.q.plan.bindValues(b.binding.s, args)
	if err != nil {
		return AuthorizedStatement{}, err
	}
	return b.binding.authorize(b.q.plan.shape, CallEach, values, secret, b.q.plan.meta, b.q.plan.scope.mode == scopeParam)
}

// One returns the single matching row, ErrNotFound on 0 rows, or ErrMultipleRows on >1 rows.
func (b *BoundSQLQuery[Row, Args, S]) One(ctx context.Context, args Args) (Row, error) {
	call, err := b.prepare(&args)
	if err != nil {
		var zero Row
		return zero, err
	}
	row, _, err := execSQLOne(ctx, &b.binding, b.q.row, b.q.plan.name, b.q.plan.meta, call, false)
	return row, err
}

// MaybeOne returns the single matching row and true, (zero, false, nil) on 0 rows,
// or ErrMultipleRows on >1 rows.
func (b *BoundSQLQuery[Row, Args, S]) MaybeOne(ctx context.Context, args Args) (Row, bool, error) {
	call, err := b.prepare(&args)
	if err != nil {
		var zero Row
		return zero, false, err
	}
	return execSQLOne(ctx, &b.binding, b.q.row, b.q.plan.name, b.q.plan.meta, call, true)
}

// First returns the first matching row and true, or (zero, false, nil) on 0 rows.
func (b *BoundSQLQuery[Row, Args, S]) First(ctx context.Context, args Args) (Row, bool, error) {
	call, err := b.prepare(&args)
	if err != nil {
		var zero Row
		return zero, false, err
	}
	return execSQLFirst(ctx, &b.binding, b.q.row, b.q.plan.name, b.q.plan.meta, call)
}

// All decodes and returns all matching rows.
func (b *BoundSQLQuery[Row, Args, S]) All(ctx context.Context, args Args) ([]Row, error) {
	call, err := b.prepare(&args)
	if err != nil {
		return nil, err
	}
	return execSQLAll(ctx, &b.binding, b.q.row, b.q.plan.name, "all", b.q.plan.meta, call)
}

// Each streams every matching row to fn, stopping on the first error returned by fn.
func (b *BoundSQLQuery[Row, Args, S]) Each(ctx context.Context, args Args, fn func(Row) error) error {
	call, err := b.prepare(&args)
	if err != nil {
		return err
	}
	return execSQLEach(ctx, &b.binding, b.q.row, b.q.plan.name, "each", b.q.plan.meta, call, fn)
}
