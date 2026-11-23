// Package stdsql provides ready-to-use database/sql transaction sessions,
// scope/RLS plug-ins, savepoints, and read-only Direct pools for tabula.
// It depends only on the Go standard library and github.com/0xJean/tabula.
package stdsql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/0xJean/tabula"
)

// ScopePolicy plugs application-specific isolation rules (scope GUC variables,
// SET LOCAL ROLE, statement timeouts, or SQL guards) into a Runner and its
// Sessions without coupling tabula to any particular domain model.
type ScopePolicy[S any] struct {
	// SetupTx runs once right after BeginTx inside the transaction (for example
	// calling SetLocalRole and SetLocalConfig for PostgreSQL RLS).
	SetupTx func(ctx context.Context, tx *sql.Tx, scope S) error
	// GuardQuery optionally inspects every SQL string before execution and
	// rejects forbidden statements (e.g. SET ROLE or set_config from adapters).
	GuardQuery func(query string) error
	// IsUnscoped reports whether scope represents an unscoped (cross-partition) call.
	IsUnscoped func(scope S) bool
}

// NoScopePolicy returns an empty ScopePolicy for applications that do not use
// row-level tenant scoping.
func NoScopePolicy() ScopePolicy[struct{}] { return ScopePolicy[struct{}]{} }

// SetLocalConfig sets a PostgreSQL transaction-local GUC via set_config(..., true).
func SetLocalConfig(ctx context.Context, tx *sql.Tx, key, value string) error {
	if key == "" {
		return errors.New("stdsql.SetLocalConfig: key is required")
	}
	_, err := tx.ExecContext(ctx, `SELECT set_config($1, $2, true)`, key, value)
	return err
}

// SetLocalRole switches the PostgreSQL role for the duration of tx.
func SetLocalRole(ctx context.Context, tx *sql.Tx, role string) error {
	if role == "" {
		return errors.New("stdsql.SetLocalRole: role is required")
	}
	_, err := tx.ExecContext(ctx, `SET LOCAL ROLE `+quoteIdent(role))
	return err
}

func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// Session implements tabula.Session[S] over one database/sql *sql.Tx.
type Session[S any] struct {
	readQueries
	tx       *sql.Tx
	scope    S
	unscoped bool
	readOnly bool
}

// NewSession wraps an existing *sql.Tx with scope and policy.
func NewSession[S any](tx *sql.Tx, scope S, policy ScopePolicy[S]) *Session[S] {
	if tx == nil {
		panic("stdsql.NewSession: tx is required")
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

// Tx returns the underlying *sql.Tx for low-level escape hatches.
func (s *Session[S]) Tx() *sql.Tx { return s.tx }

// Scope returns the session's bound scope value.
func (s *Session[S]) Scope() S { return s.scope }

// Unscoped reports whether this session bypasses row-level scope predicates.
func (s *Session[S]) Unscoped() bool { return s.unscoped }

// ReadOnly reports whether this session was opened as a READ ONLY transaction.
func (s *Session[S]) ReadOnly() bool { return s.readOnly }

func wrapReadOnlyErr(err error) error {
	if tabula.SQLState(err) == "25006" {
		return errors.Join(tabula.ErrReadOnly, err)
	}
	return err
}

func (s readQueries) check(query string) error {
	if s.guard != nil {
		return s.guard(query)
	}
	return nil
}

// Exec executes a statement and returns its affected row count.
func (s *Session[S]) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	if err := s.check(query); err != nil {
		return 0, err
	}
	res, err := s.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, wrapReadOnlyErr(err)
	}
	return res.RowsAffected()
}

// Scan executes a single-row query and scans into dest.
func (s readQueries) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	if err := s.check(query); err != nil {
		return err
	}
	return wrapReadOnlyErr(s.db.QueryRowContext(ctx, query, args...).Scan(dest...))
}

// Returning executes a write with RETURNING and scans the returned row into dest.
func (s *Session[S]) Returning(ctx context.Context, query string, args []any, dest ...any) error {
	return s.Scan(ctx, query, args, dest...)
}

// Each streams all rows of query through decode.
func (s readQueries) Each(ctx context.Context, query string, args []any, decode func(func(...any) error) error) error {
	if err := s.check(query); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return wrapReadOnlyErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := decode(rows.Scan); err != nil {
			return err
		}
	}
	return wrapReadOnlyErr(rows.Err())
}

// Savepoint runs fn inside a PostgreSQL SAVEPOINT. On error or panic, changes
// made inside fn are rolled back to the savepoint while keeping the enclosing
// transaction usable; on success the savepoint is released.
func (s *Session[S]) Savepoint(ctx context.Context, name string, fn func(context.Context) error) (err error) {
	if name == "" {
		return errors.New("stdsql.Savepoint: name is required")
	}
	quoted := quoteIdent(name)
	if _, err := s.tx.ExecContext(ctx, "SAVEPOINT "+quoted); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if p := recover(); p != nil {
			_, _ = s.tx.ExecContext(context.WithoutCancel(ctx), "ROLLBACK TO SAVEPOINT "+quoted)
			_, _ = s.tx.ExecContext(context.WithoutCancel(ctx), "RELEASE SAVEPOINT "+quoted)
			panic(p)
		}
		_, rbErr := s.tx.ExecContext(context.WithoutCancel(ctx), "ROLLBACK TO SAVEPOINT "+quoted)
		_, relErr := s.tx.ExecContext(context.WithoutCancel(ctx), "RELEASE SAVEPOINT "+quoted)
		if err == nil {
			err = errors.Join(rbErr, relErr)
		}
	}()
	if err := fn(ctx); err != nil {
		return err
	}
	if _, err := s.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+quoted); err != nil {
		return err
	}
	committed = true
	return nil
}

// Runner opens scoped PostgreSQL transactions on a *sql.DB and binds them to
// Session[S] handles.
type Runner[S any] struct {
	db     *sql.DB
	policy ScopePolicy[S]
}

// NewRunner creates a Runner backed by db and policy.
func NewRunner[S any](db *sql.DB, policy ScopePolicy[S]) *Runner[S] {
	if db == nil {
		panic("stdsql.NewRunner: db is required")
	}
	return &Runner[S]{db: db, policy: policy}
}

// Within runs fn in a read-write transaction with the database default isolation.
func (r *Runner[S]) Within(ctx context.Context, scope S, fn func(context.Context, *Session[S]) error) error {
	return r.WithinOptions(ctx, scope, nil, fn)
}

// WithinReadOnly runs fn in a READ ONLY transaction.
func (r *Runner[S]) WithinReadOnly(ctx context.Context, scope S, fn func(context.Context, *Session[S]) error) error {
	return r.WithinOptions(ctx, scope, &sql.TxOptions{ReadOnly: true}, fn)
}

// WithinIsolation runs fn in a transaction with the requested isolation level.
func (r *Runner[S]) WithinIsolation(ctx context.Context, scope S, level sql.IsolationLevel, fn func(context.Context, *Session[S]) error) error {
	return r.WithinOptions(ctx, scope, &sql.TxOptions{Isolation: level}, fn)
}

// WithinOptions runs fn inside a transaction started with opts. SetupTx runs
// immediately after BeginTx. Errors and panics roll back automatically.
func (r *Runner[S]) WithinOptions(ctx context.Context, scope S, opts *sql.TxOptions, fn func(context.Context, *Session[S]) error) (err error) {
	tx, err := r.db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("stdsql: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if r.policy.SetupTx != nil {
		if err := r.policy.SetupTx(ctx, tx, scope); err != nil {
			return err
		}
	}
	sess := NewSession(tx, scope, r.policy)
	if opts != nil && opts.ReadOnly {
		sess.readOnly = true
	}
	if err := fn(ctx, sess); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("stdsql: commit tx: %w", err)
	}
	committed = true
	return nil
}
