// Package stdsql provides role-bound database/sql transaction runners, guarded
// sessions, scope/RLS policies, savepoints, and read-only Direct pools for
// tabula. It depends only on the Go standard library and github.com/0xJean/tabula.
package stdsql

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/0xJean/tabula"
)

// Compile-time check that *Session[S] and *RawSession[S] implement tabula.Session[S].
var (
	_ tabula.Session[struct{}] = (*Session[struct{}])(nil)
	_ tabula.Session[struct{}] = (*RawSession[struct{}])(nil)
)

// SetLocalConfig sets a PostgreSQL transaction-local GUC via set_config(..., true).
func SetLocalConfig(ctx context.Context, tx *sql.Tx, key, value string) error {
	if key == "" {
		return errors.New("stdsql.SetLocalConfig: key is required")
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_catalog.set_config($1, $2, true)`, key, value)
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

// Session implements tabula.Session[S] over one database/sql *sql.Tx without
// exposing the raw transaction. Overlapping statements or savepoint requests
// on the transaction return tabula.ErrSessionBusy, including inside Each.
type Session[S any] struct {
	mu     *sync.Mutex // shared by handles on the same transaction
	closed atomic.Bool
	readQueries
	tx       *sql.Tx
	scope    S
	unscoped bool
	readOnly bool
	denied   error
	proof    tabula.SessionProof
	obs      *tabula.Observer
	scopeLog func(S) string
	shared   *sharedState[S]
}

// RawSession wraps a *Session[S] with explicit raw *sql.Tx escape access. It is
// constructed only when RunnerOptions.ExposeTx is true via Runner.WithinRaw.
type RawSession[S any] struct {
	sess  *Session[S]
	proof tabula.SessionProof
}

// Tx returns the underlying *sql.Tx. Only available on *RawSession[S].
func (s *RawSession[S]) Tx() *sql.Tx { return s.sess.tx }

// Session returns the underlying guarded *Session[S].
func (s *RawSession[S]) Session() *Session[S] { return s.sess }

// SessionProof returns the instance-bound proof for this *RawSession[S].
func (s *RawSession[S]) SessionProof() tabula.SessionProof { return s.proof }

// TabulaObserved reports which observability signals this session owns.
func (s *RawSession[S]) TabulaObserved() tabula.ObserveMask { return s.sess.TabulaObserved() }

// Scope returns the session's bound scope value.
func (s *RawSession[S]) Scope() S { return s.sess.Scope() }

// Unscoped reports whether this session bypasses row-level scope predicates.
func (s *RawSession[S]) Unscoped() bool { return s.sess.Unscoped() }

// ReadOnly reports whether this session was opened as a READ ONLY transaction.
func (s *RawSession[S]) ReadOnly() bool { return s.sess.ReadOnly() }

// Exec executes a statement and returns its affected row count.
func (s *RawSession[S]) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	return s.sess.Exec(ctx, query, args...)
}

// Scan executes a single-row query and scans into dest.
func (s *RawSession[S]) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	return s.sess.Scan(ctx, query, args, dest...)
}

// Returning executes a write with RETURNING and scans the returned row into dest.
func (s *RawSession[S]) Returning(ctx context.Context, query string, args []any, dest ...any) error {
	return s.sess.Returning(ctx, query, args, dest...)
}

// Each streams all rows of query through decode.
func (s *RawSession[S]) Each(ctx context.Context, query string, args []any, decode func(func(...any) error) error) error {
	return s.sess.Each(ctx, query, args, decode)
}

// Savepoint runs fn inside a PostgreSQL SAVEPOINT on the underlying transaction.
func (s *RawSession[S]) Savepoint(ctx context.Context, name string, fn func(context.Context) error) error {
	return s.sess.Savepoint(ctx, name, fn)
}

// NewSession wraps an existing *sql.Tx with scope and policy as an unsealed
// migration handle. Keyed registries require a Runner or keyed DirectPool.
func NewSession[S any](tx *sql.Tx, scope S, policy ScopePolicy[S]) *Session[S] {
	if tx == nil {
		panic("stdsql.NewSession: tx is required")
	}
	unscoped := false
	if policy.IsUnscoped != nil {
		unscoped = policy.IsUnscoped(scope)
	}
	return &Session[S]{
		mu:          new(sync.Mutex),
		tx:          tx,
		scope:       scope,
		unscoped:    unscoped,
		scopeLog:    policy.ScopeLog,
		readQueries: readQueries{db: tx, guard: policy.GuardQuery},
	}
}

func (s *Session[S]) close() {
	s.mu.Lock()
	s.closed.Store(true)
	s.mu.Unlock()
}

// Scope returns the session's bound scope value.
func (s *Session[S]) Scope() S { return s.scope }

// Unscoped reports whether this session bypasses row-level scope predicates.
func (s *Session[S]) Unscoped() bool { return s.unscoped }

// ReadOnly reports whether this session was opened as a READ ONLY transaction.
func (s *Session[S]) ReadOnly() bool { return s.readOnly }

// SessionProof returns the instance-bound proof for A4 sealed registries.
func (s *Session[S]) SessionProof() tabula.SessionProof { return s.proof }

// TabulaObserved reports which observability signals this session owns.
func (s *Session[S]) TabulaObserved() tabula.ObserveMask { return s.obs.Signals() }

func (s *Session[S]) scopeLabel() string {
	if s.scopeLog == nil {
		return ""
	}
	return s.scopeLog(s.scope)
}

// tryLock reserves the transaction for one operation, including row callbacks.
func (s *Session[S]) tryLock() error {
	if s.mu == nil {
		return tabula.ErrSessionClosed
	}
	locked := s.mu.TryLock()
	// A closed or denied handle keeps its error while a sibling holds the lock.
	if err := s.checkState(); err != nil {
		if locked {
			s.mu.Unlock()
		}
		return err
	}
	if !locked {
		return tabula.ErrSessionBusy
	}
	return nil
}

func (s *Session[S]) checkState() error {
	if s.closed.Load() || (s.shared != nil && s.shared.closed.Load()) {
		return tabula.ErrSessionClosed
	}
	return s.denied
}

func (s *Session[S]) applyScope(ctx context.Context) error {
	if s.shared == nil {
		return nil
	}
	return s.shared.applyFor(ctx, s.scope, s.unscoped)
}

func wrapReadOnlyErr(err error) error {
	if tabula.SQLState(err) == "25006" {
		return errors.Join(tabula.ErrReadOnly, err)
	}
	return err
}

func (s *Session[S]) check(query string) error {
	if s.readOnly {
		if err := tabula.GuardReadOnlyTransaction(query); err != nil {
			return err
		}
	}
	return s.readQueries.check(query)
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
	if err := s.tryLock(); err != nil {
		return 0, err
	}
	defer s.mu.Unlock()
	if err := s.applyScope(ctx); err != nil {
		return 0, err
	}
	tagged := s.obs.TagSQL(ctx, query)
	opCtx, done := s.obs.Start(ctx, s.scopeLabel(), query, args)
	res, err := s.tx.ExecContext(opCtx, tagged, args...)
	if err != nil {
		err = wrapReadOnlyErr(err)
		done(0, err)
		return 0, err
	}
	n, err := res.RowsAffected()
	done(n, err)
	return n, err
}

// Scan executes a single-row query and scans into dest.
func (s *Session[S]) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	if err := s.check(query); err != nil {
		return err
	}
	if err := s.tryLock(); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if err := s.applyScope(ctx); err != nil {
		return err
	}
	tagged := s.obs.TagSQL(ctx, query)
	opCtx, done := s.obs.Start(ctx, s.scopeLabel(), query, args)
	err := wrapReadOnlyErr(s.tx.QueryRowContext(opCtx, tagged, args...).Scan(dest...))
	done(-1, err)
	return err
}

// Returning executes a write with RETURNING and scans the returned row into dest.
func (s *Session[S]) Returning(ctx context.Context, query string, args []any, dest ...any) error {
	return s.Scan(ctx, query, args, dest...)
}

// Each streams all rows of query through decode.
func (s *Session[S]) Each(ctx context.Context, query string, args []any, decode func(func(...any) error) error) error {
	if err := s.check(query); err != nil {
		return err
	}
	if err := s.tryLock(); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if err := s.applyScope(ctx); err != nil {
		return err
	}
	tagged := s.obs.TagSQL(ctx, query)
	opCtx, done := s.obs.Start(ctx, s.scopeLabel(), query, args)
	rows, err := s.tx.QueryContext(opCtx, tagged, args...)
	if err != nil {
		err = wrapReadOnlyErr(err)
		done(0, err)
		return err
	}
	defer func() { _ = rows.Close() }()
	var count int64
	for rows.Next() {
		if err := decode(rows.Scan); err != nil {
			done(count, err)
			return err
		}
		count++
	}
	err = wrapReadOnlyErr(rows.Err())
	done(count, err)
	return err
}

// Scan executes a single-row query on the direct read-only queryer.
func (s readQueries) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	if err := s.check(query); err != nil {
		return err
	}
	return wrapReadOnlyErr(s.db.QueryRowContext(ctx, query, args...).Scan(dest...))
}

// Each streams all rows of query on the direct read-only queryer.
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
	start := func() error {
		if err := s.tryLock(); err != nil {
			return err
		}
		defer s.mu.Unlock()
		_, err := s.tx.ExecContext(ctx, "SAVEPOINT "+quoted)
		return err
	}
	if err := start(); err != nil {
		return err
	}
	// Once the savepoint exists, finish it even if another statement is
	// completing on the transaction.
	execSP := func(spCtx context.Context, stmt string) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.checkState(); err != nil {
			return err
		}
		_, err := s.tx.ExecContext(spCtx, stmt)
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if p := recover(); p != nil {
			_ = execSP(context.WithoutCancel(ctx), "ROLLBACK TO SAVEPOINT "+quoted)
			_ = execSP(context.WithoutCancel(ctx), "RELEASE SAVEPOINT "+quoted)
			panic(p)
		}
		rbErr := execSP(context.WithoutCancel(ctx), "ROLLBACK TO SAVEPOINT "+quoted)
		relErr := execSP(context.WithoutCancel(ctx), "RELEASE SAVEPOINT "+quoted)
		if err == nil {
			err = errors.Join(rbErr, relErr)
		}
	}()
	if err := fn(ctx); err != nil {
		return err
	}
	if err := execSP(ctx, "RELEASE SAVEPOINT "+quoted); err != nil {
		return err
	}
	committed = true
	return nil
}
