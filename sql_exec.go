package tabula

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// SQLExec is a compiled, registered row-count-only named-parameter SQL mutation
// (or explicit transaction-level advisory lock) binding parameters from Args.
type SQLExec[Args, S any] struct {
	plan         *compiledSQLPlan[Args, S]
	advisoryLock bool
}

// DefineSQLExec validates and registers a non-returning write statement
// (INSERT, UPDATE, DELETE, MERGE, or modifying CTE without top-level RETURNING)
// or an explicit AdvisoryLock transaction-level advisory lock call on r.
// Panics on declaration mistakes at initialization.
func DefineSQLExec[Args, S any](r *Registry[S], spec SQLSpec) *SQLExec[Args, S] {
	q, err := TryDefineSQLExec[Args](r, spec)
	if err != nil {
		panic(err)
	}
	return q
}

// TryDefineSQLExec validates and atomically registers a non-returning write or
// transaction-level advisory lock on r, publishing nothing on error.
func TryDefineSQLExec[Args, S any](r *Registry[S], spec SQLSpec) (*SQLExec[Args, S], error) {
	plan, err := compileSQLPlan[Args](r, spec, sqlKindExec, "DefineSQLExec", nil, nil)
	if err != nil {
		return nil, err
	}
	return &SQLExec[Args, S]{plan: plan, advisoryLock: spec.AdvisoryLock}, nil
}

// Name returns the registered declaration name.
func (q *SQLExec[Args, S]) Name() string { return q.plan.name }

// SQL returns the compiled positional PostgreSQL SQL ($1..$n).
func (q *SQLExec[Args, S]) SQL() string { return q.plan.shape.SQL }

// Definition returns a deep copy of the registered Definition for sqlcheck.
func (q *SQLExec[Args, S]) Definition() Definition { return q.plan.definition() }

// Bind attaches this statement to an authorized session from the owning Registry.
func (q *SQLExec[Args, S]) Bind(s Session[S]) *BoundSQLExec[Args, S] {
	if q == nil || q.plan == nil {
		return &BoundSQLExec[Args, S]{binding: sqlSessionBinding[S]{bindErr: ErrStatementAuthority}}
	}
	return &BoundSQLExec[Args, S]{q: q, binding: bindSQLSession(q.plan.reg, s)}
}

// BoundSQLExec executes a registered SQLExec on one authorized session.
type BoundSQLExec[Args, S any] struct {
	q       *SQLExec[Args, S]
	binding sqlSessionBinding[S]
}

// WithLogger returns a handle with a per-binding logger override.
func (b *BoundSQLExec[Args, S]) WithLogger(logger *slog.Logger) *BoundSQLExec[Args, S] {
	clone := *b
	clone.binding.logger = logger
	return &clone
}

// Exec executes the statement and returns the affected row count.
func (b *BoundSQLExec[Args, S]) Exec(ctx context.Context, args Args) (int64, error) {
	if b == nil || b.q == nil || b.q.plan == nil {
		return 0, ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return 0, b.binding.bindErr
	}
	values, secret, err := b.q.plan.bindValues(b.binding.s, &args)
	if err != nil {
		return 0, err
	}
	method := CallExec
	if b.q.advisoryLock {
		method = CallEach
	}
	call, err := b.binding.authorize(b.q.plan.shape, method, values, secret, b.q.plan.meta, b.q.plan.scope.mode == scopeParam)
	if err != nil {
		return 0, err
	}
	if b.q.advisoryLock {
		return b.binding.runSQL(ctx, b.q.plan.name, "exec", b.q.plan.meta, call, func(ctx context.Context) (int64, error) {
			var count int64
			err := b.binding.driver.EachAuthorized(ctx, call, func(func(...any) error) error {
				count++
				return nil
			})
			return count, err
		})
	}
	return b.binding.runSQL(ctx, b.q.plan.name, "exec", b.q.plan.meta, call, func(ctx context.Context) (int64, error) {
		return b.binding.driver.ExecAuthorized(ctx, call)
	})
}

// SQLReturning is a compiled, registered row-returning named-parameter SQL
// write (INSERT/UPDATE/DELETE/MERGE ... RETURNING ... or a modifying CTE
// returning rows) scanning results into Row and binding parameters from Args.
type SQLReturning[Row, Args, S any] struct {
	plan *compiledSQLPlan[Args, S]
	row  *descriptor[Row, S]
}

// DefineSQLReturning validates and registers a returning write statement on r.
// Panics on declaration mistakes at initialization.
func DefineSQLReturning[Row, Args, S any](r *Registry[S], spec SQLSpec) *SQLReturning[Row, Args, S] {
	q, err := TryDefineSQLReturning[Row, Args](r, spec)
	if err != nil {
		panic(err)
	}
	return q
}

// TryDefineSQLReturning validates and atomically registers a returning write
// statement on r, publishing nothing on error.
func TryDefineSQLReturning[Row, Args, S any](r *Registry[S], spec SQLSpec) (*SQLReturning[Row, Args, S], error) {
	if r == nil {
		return nil, errors.New("tabula.DefineSQLReturning: registry is required")
	}
	name := strings.TrimSpace(spec.Name)
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s.DefineSQLReturning %s: "+format, append([]any{r.prefix, name}, args...)...)
	}
	rowDesc, resultFields, err := compileSQLRow[Row](r, name, fail)
	if err != nil {
		return nil, err
	}
	plan, err := compileSQLPlan[Args](r, spec, sqlKindReturning, "DefineSQLReturning", resultFields, rowDesc.definedColumns())
	if err != nil {
		return nil, err
	}
	return &SQLReturning[Row, Args, S]{plan: plan, row: rowDesc}, nil
}

// Name returns the registered declaration name.
func (q *SQLReturning[Row, Args, S]) Name() string { return q.plan.name }

// SQL returns the compiled positional PostgreSQL SQL ($1..$n).
func (q *SQLReturning[Row, Args, S]) SQL() string { return q.plan.shape.SQL }

// Definition returns a deep copy of the registered Definition for sqlcheck.
func (q *SQLReturning[Row, Args, S]) Definition() Definition { return q.plan.definition() }

// Bind attaches this statement to an authorized session from the owning Registry.
func (q *SQLReturning[Row, Args, S]) Bind(s Session[S]) *BoundSQLReturning[Row, Args, S] {
	if q == nil || q.plan == nil {
		return &BoundSQLReturning[Row, Args, S]{binding: sqlSessionBinding[S]{bindErr: ErrStatementAuthority}}
	}
	return &BoundSQLReturning[Row, Args, S]{q: q, binding: bindSQLSession(q.plan.reg, s)}
}

// BoundSQLReturning executes a registered SQLReturning on one authorized session.
type BoundSQLReturning[Row, Args, S any] struct {
	q       *SQLReturning[Row, Args, S]
	binding sqlSessionBinding[S]
}

// WithLogger returns a handle with a per-binding logger override.
func (b *BoundSQLReturning[Row, Args, S]) WithLogger(logger *slog.Logger) *BoundSQLReturning[Row, Args, S] {
	clone := *b
	clone.binding.logger = logger
	return &clone
}

func (b *BoundSQLReturning[Row, Args, S]) prepare(args *Args) (AuthorizedStatement, error) {
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
	return b.binding.authorize(b.q.plan.shape, CallReturningEach, values, secret, b.q.plan.meta, b.q.plan.scope.mode == scopeParam)
}

// One executes the write and returns the single returned row, ErrNotFound on 0
// rows, or ErrMultipleRows on >1 rows.
func (b *BoundSQLReturning[Row, Args, S]) One(ctx context.Context, args Args) (Row, error) {
	call, err := b.prepare(&args)
	if err != nil {
		var zero Row
		return zero, err
	}
	row, _, err := execSQLOne(ctx, &b.binding, b.q.row, b.q.plan.name, b.q.plan.meta, call, false)
	return row, err
}

// MaybeOne executes the write and returns the single returned row and true,
// (zero, false, nil) on 0 rows, or ErrMultipleRows on >1 rows.
func (b *BoundSQLReturning[Row, Args, S]) MaybeOne(ctx context.Context, args Args) (Row, bool, error) {
	call, err := b.prepare(&args)
	if err != nil {
		var zero Row
		return zero, false, err
	}
	return execSQLOne(ctx, &b.binding, b.q.row, b.q.plan.name, b.q.plan.meta, call, true)
}

// All executes the write and decodes every returned row.
func (b *BoundSQLReturning[Row, Args, S]) All(ctx context.Context, args Args) ([]Row, error) {
	call, err := b.prepare(&args)
	if err != nil {
		return nil, err
	}
	return execSQLAll(ctx, &b.binding, b.q.row, b.q.plan.name, "returning_all", b.q.plan.meta, call)
}

// Each executes the write and streams every returned row to fn.
func (b *BoundSQLReturning[Row, Args, S]) Each(ctx context.Context, args Args, fn func(Row) error) error {
	call, err := b.prepare(&args)
	if err != nil {
		return err
	}
	return execSQLEach(ctx, &b.binding, b.q.row, b.q.plan.name, "returning_each", b.q.plan.meta, call, fn)
}
