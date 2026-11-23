// Package pgxdriver integrates github.com/jackc/pgx/v5 with tabula:
// PostgreSQL array column converters, transaction-bound
// Sessions, and pgxpool Runners with pluggable scope/RLS policies and savepoints.
package pgxdriver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/0xJean/tabula"
)

// Compile-time check that *Session[S] implements tabula.Session[S].
var _ tabula.Session[struct{}] = (*Session[struct{}])(nil)

// ArrayColumn selects pgx's native one-dimensional array codec. Its type map
// handles both built-in and named Go slices and element types.
func ArrayColumn(ft reflect.Type) (func(any) any, error) {
	if ft.Kind() != reflect.Slice {
		return nil, fmt.Errorf("array option requires a slice type, got %s", ft)
	}
	switch ft.Elem().Kind() {
	case reflect.String, reflect.Int64, reflect.Int32, reflect.Float64, reflect.Bool:
		return func(p any) any { return p }, nil
	default:
		return nil, fmt.Errorf("unsupported array element type %s", ft.Elem())
	}
}

// ScopePolicy plugs application-specific isolation rules (scope GUC variables,
// SET LOCAL ROLE, or SQL guards) into a pgx Runner and its Sessions.
type ScopePolicy[S any] struct {
	SetupTx    func(ctx context.Context, tx pgx.Tx, scope S) error
	GuardQuery func(query string) error
	IsUnscoped func(scope S) bool
}

// NoScopePolicy returns an empty ScopePolicy for applications without row-level scoping.
func NoScopePolicy() ScopePolicy[struct{}] { return ScopePolicy[struct{}]{} }

// SetLocalConfig sets a PostgreSQL transaction-local GUC via set_config(..., true).
func SetLocalConfig(ctx context.Context, tx pgx.Tx, key, value string) error {
	if key == "" {
		return errors.New("pgxdriver.SetLocalConfig: key is required")
	}
	_, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, key, value)
	return err
}

// SetLocalRole switches the PostgreSQL role for the duration of tx.
func SetLocalRole(ctx context.Context, tx pgx.Tx, role string) error {
	if role == "" {
		return errors.New("pgxdriver.SetLocalRole: role is required")
	}
	_, err := tx.Exec(ctx, `SET LOCAL ROLE `+quoteIdent(role))
	return err
}

func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// Session implements tabula.Session[S] over one pgx.Tx.
type Session[S any] struct {
	readQueries
	tx       pgx.Tx
	scope    S
	unscoped bool
	readOnly bool
}

// NewSession wraps an existing pgx.Tx with scope and policy.
func NewSession[S any](tx pgx.Tx, scope S, policy ScopePolicy[S]) *Session[S] {
	if tx == nil {
		panic("pgxdriver.NewSession: tx is required")
	}
	unscoped := false
	if policy.IsUnscoped != nil {
		unscoped = policy.IsUnscoped(scope)
	}
	return &Session[S]{
		tx:          tx,
		scope:       scope,
		unscoped:    unscoped,
		readQueries: readQueries{db: tx, guard: policy.GuardQuery},
	}
}

// Tx returns the underlying pgx.Tx for low-level escape hatches.
func (s *Session[S]) Tx() pgx.Tx { return s.tx }

// Scope returns the session's bound scope value.
func (s *Session[S]) Scope() S { return s.scope }

// Unscoped reports whether this session bypasses row-level scope predicates.
func (s *Session[S]) Unscoped() bool { return s.unscoped }

// ReadOnly reports whether this session was opened as a READ ONLY transaction.
func (s *Session[S]) ReadOnly() bool { return s.readOnly }

func (s readQueries) check(query string) error {
	if s.guard != nil {
		return s.guard(query)
	}
	return nil
}

// Exec executes query and returns the affected row count.
func (s *Session[S]) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	if err := s.check(query); err != nil {
		return 0, err
	}
	tag, err := s.tx.Exec(ctx, query, args...)
	if err != nil {
		return 0, directReadError(err)
	}
	return tag.RowsAffected(), nil
}

// Scan executes a single-row query and maps pgx.ErrNoRows to sql.ErrNoRows.
func (s readQueries) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	if err := s.check(query); err != nil {
		return err
	}
	err := s.db.QueryRow(ctx, query, args...).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return sql.ErrNoRows
	}
	return directReadError(err)
}

// Returning executes a write with RETURNING and maps pgx.ErrNoRows to sql.ErrNoRows.
func (s *Session[S]) Returning(ctx context.Context, query string, args []any, dest ...any) error {
	return s.Scan(ctx, query, args, dest...)
}

// Each streams all rows of query through decode.
func (s readQueries) Each(ctx context.Context, query string, args []any, decode func(func(...any) error) error) error {
	if err := s.check(query); err != nil {
		return err
	}
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return directReadError(err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := decode(rows.Scan); err != nil {
			return err
		}
	}
	return directReadError(rows.Err())
}

// Savepoint runs fn inside a nested pgx transaction (PostgreSQL SAVEPOINT).
func (s *Session[S]) Savepoint(ctx context.Context, fn func(context.Context, *Session[S]) error) error {
	nested, err := s.tx.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = nested.Rollback(context.WithoutCancel(ctx))
		}
	}()
	child := &Session[S]{tx: nested, scope: s.scope, unscoped: s.unscoped, readOnly: s.readOnly, readQueries: readQueries{db: nested, guard: s.guard}}
	if err := fn(ctx, child); err != nil {
		return err
	}
	if err := nested.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// Runner opens scoped PostgreSQL transactions on a *pgxpool.Pool.
type Runner[S any] struct {
	pool   *pgxpool.Pool
	policy ScopePolicy[S]
}

// NewRunner creates a Runner backed by pool and policy.
func NewRunner[S any](pool *pgxpool.Pool, policy ScopePolicy[S]) *Runner[S] {
	if pool == nil {
		panic("pgxdriver.NewRunner: pool is required")
	}
	return &Runner[S]{pool: pool, policy: policy}
}

// Within runs fn in a read-write transaction.
func (r *Runner[S]) Within(ctx context.Context, scope S, fn func(context.Context, *Session[S]) error) error {
	return r.WithinTxOptions(ctx, scope, pgx.TxOptions{}, fn)
}

// WithinReadOnly runs fn in a READ ONLY transaction.
func (r *Runner[S]) WithinReadOnly(ctx context.Context, scope S, fn func(context.Context, *Session[S]) error) error {
	return r.WithinTxOptions(ctx, scope, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn)
}

// WithinTxOptions runs fn inside a transaction opened with opts.
func (r *Runner[S]) WithinTxOptions(ctx context.Context, scope S, opts pgx.TxOptions, fn func(context.Context, *Session[S]) error) error {
	tx, err := r.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("pgxdriver: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if r.policy.SetupTx != nil {
		if err := r.policy.SetupTx(ctx, tx, scope); err != nil {
			return err
		}
	}
	sess := NewSession(tx, scope, r.policy)
	if opts.AccessMode == pgx.ReadOnly {
		sess.readOnly = true
	}
	if err := fn(ctx, sess); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgxdriver: commit tx: %w", err)
	}
	committed = true
	return nil
}
