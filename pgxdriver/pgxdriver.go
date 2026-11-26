// Package pgxdriver integrates github.com/jackc/pgx/v5 with tabula:
// PostgreSQL array column converters, role-bound transaction Runners, guarded
// Sessions, savepoints, binary COPY, and read-only Direct pools.
package pgxdriver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/0xJean/tabula"
)

// Compile-time check that *Session[S] and *RawSession[S] implement tabula.Session[S].
var (
	_ tabula.Session[struct{}] = (*Session[struct{}])(nil)
	_ tabula.Session[struct{}] = (*RawSession[struct{}])(nil)
)

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

// SetLocalConfig sets a PostgreSQL transaction-local GUC via set_config(..., true).
func SetLocalConfig(ctx context.Context, tx pgx.Tx, key, value string) error {
	if key == "" {
		return errors.New("pgxdriver.SetLocalConfig: key is required")
	}
	_, err := tx.Exec(ctx, `SELECT pg_catalog.set_config($1, $2, true)`, key, value)
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

// Session implements tabula.Session[S] over one pgx.Tx without exposing the raw
// transaction. Overlapping statements or savepoint requests on the transaction
// return tabula.ErrSessionBusy, including inside Each or a binary COPY.
type Session[S any] struct {
	authority  tabula.SQLAuthority
	statements tabula.StatementGuard
	mu         *statementLock // shared by handles on the same transaction
	closed     atomic.Bool
	readQueries
	tx       pgx.Tx
	scope    S
	unscoped bool
	readOnly bool
	readRole bool
	denied   error
	key      tabula.SessionKey
	proof    tabula.SessionProof
	obs      *tabula.Observer
	scopeLog func(S) string
	shared   *sharedState[S]
}

// RawSession wraps a *Session[S] with explicit raw pgx.Tx escape access. It is
// constructed only when RunnerOptions.ExposeTx is true via Runner.WithinRaw.
type RawSession[S any] struct {
	sess  *Session[S]
	proof tabula.SessionProof
}

// Tx returns the underlying pgx.Tx. Only available on *RawSession[S].
func (s *RawSession[S]) Tx() pgx.Tx { return s.sess.tx }

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

// ReadOnly reports whether this handle permits only reads.
func (s *RawSession[S]) ReadOnly() bool { return s.sess.ReadOnly() }

// Exec executes query and returns the affected row count.
func (s *RawSession[S]) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	return s.sess.Exec(ctx, query, args...)
}

// Scan executes a single-row query and maps pgx.ErrNoRows to sql.ErrNoRows.
func (s *RawSession[S]) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	return s.sess.Scan(ctx, query, args, dest...)
}

// Returning executes a write with RETURNING and maps pgx.ErrNoRows to sql.ErrNoRows.
func (s *RawSession[S]) Returning(ctx context.Context, query string, args []any, dest ...any) error {
	return s.sess.Returning(ctx, query, args, dest...)
}

// Each streams all rows of query through decode.
func (s *RawSession[S]) Each(ctx context.Context, query string, args []any, decode func(func(...any) error) error) error {
	return s.sess.Each(ctx, query, args, decode)
}

// Savepoint runs fn inside a nested pgx transaction (PostgreSQL SAVEPOINT).
func (s *RawSession[S]) Savepoint(ctx context.Context, fn func(context.Context, *Session[S]) error) error {
	return s.sess.Savepoint(ctx, fn)
}

// NewSession wraps an existing pgx.Tx with scope and policy as an unsealed
// migration handle. Keyed registries require a Runner or keyed DirectPool.
func NewSession[S any](tx pgx.Tx, scope S, policy ScopePolicy[S]) *Session[S] {
	if tx == nil {
		panic("pgxdriver.NewSession: tx is required")
	}
	unscoped := false
	if policy.IsUnscoped != nil {
		unscoped = policy.IsUnscoped(scope)
	}
	return &Session[S]{
		mu:          new(statementLock),
		tx:          tx,
		scope:       scope,
		unscoped:    unscoped,
		scopeLog:    policy.ScopeLog,
		readQueries: readQueries{db: tx, guard: policy.GuardQuery},
	}
}

func (s *Session[S]) close() {
	s.mu.close()
}

// Scope returns the session's bound scope value.
func (s *Session[S]) Scope() S { return s.scope }

// Unscoped reports whether this session bypasses row-level scope predicates.
func (s *Session[S]) Unscoped() bool { return s.unscoped }

// ReadOnly reports whether this handle permits only reads.
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
	if s.mu.cursor != nil {
		s.mu.Unlock()
		return tabula.ErrSessionBusy
	}
	return nil
}

func (s *Session[S]) checkContext(ctx context.Context) error {
	if s.mu == nil || s.mu.checkCallback == nil {
		return nil
	}
	if err := s.checkState(); err != nil {
		return err
	}
	return s.mu.checkCallback(ctx)
}

func (s *Session[S]) checkState() error {
	if s.mu == nil || s.mu.closed.Load() || s.closed.Load() {
		return tabula.ErrSessionClosed
	}
	return s.denied
}

func (s *Session[S]) applyScope(ctx context.Context) error {
	if s.shared == nil {
		return nil
	}
	return s.shared.applyFor(ctx, s.tx, s.scope, s.unscoped)
}

func (s *Session[S]) check(method, query string, args []any) error {
	if err := s.checkReadOnly(query); err != nil {
		return err
	}
	if s.statements.Enabled() {
		for _, arg := range args {
			if _, ok := arg.(pgx.QueryRewriter); ok {
				return fmt.Errorf("%w: statement policy requires positional bind values", tabula.ErrForbiddenStatement)
			}
		}
	}
	if err := s.statements.Check(method, query); err != nil {
		return err
	}
	return s.readQueries.check(query)
}

func (s *Session[S]) checkReadOnly(query string) error {
	if s.readRole {
		return tabula.GuardReadOnly(query)
	}
	if s.readOnly {
		return tabula.GuardReadOnlyTransaction(query)
	}
	return nil
}

func (s readQueries) check(query string) error {
	if s.guard != nil {
		return s.guard(query)
	}
	return nil
}

func (s *Session[S]) exec(ctx context.Context, query string, args []any) (int64, error) {
	if err := s.checkContext(ctx); err != nil {
		return 0, err
	}
	if s.readRole {
		if err := s.checkState(); err != nil {
			return 0, err
		}
		return 0, tabula.ErrReadOnly
	}
	if err := s.check("Exec", query, args); err != nil {
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
	tag, err := s.tx.Exec(opCtx, tagged, args...)
	if err != nil {
		err = directReadError(err)
		done(0, err)
		return 0, err
	}
	n := tag.RowsAffected()
	done(n, nil)
	return n, nil
}

func (s *Session[S]) scan(ctx context.Context, method, query string, args []any, dest ...any) error {
	if err := s.checkContext(ctx); err != nil {
		return err
	}
	if err := s.check(method, query, args); err != nil {
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
	err := s.tx.QueryRow(opCtx, tagged, args...).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		err = sql.ErrNoRows
	} else {
		err = directReadError(err)
	}
	done(-1, err)
	return err
}

func (s *Session[S]) returning(ctx context.Context, query string, args []any, dest ...any) error {
	if s.readRole {
		if err := s.checkContext(ctx); err != nil {
			return err
		}
		if err := s.checkState(); err != nil {
			return err
		}
		return tabula.ErrReadOnly
	}
	return s.scan(ctx, "Returning", query, args, dest...)
}

func (s *Session[S]) each(ctx context.Context, method, query string, args []any, decode func(func(...any) error) error) error {
	if err := s.checkContext(ctx); err != nil {
		return err
	}
	if err := s.check(method, query, args); err != nil {
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
	rows, err := s.tx.Query(opCtx, tagged, args...)
	if err != nil {
		err = directReadError(err)
		done(0, err)
		return err
	}
	return s.mu.each(rows, decode, done)
}

// Scan executes a single-row query on the direct read-only queryer.
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

// Each streams all rows of query on the direct read-only queryer.
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
			return callbackReadError(rows, err)
		}
	}
	return directReadError(rows.Err())
}

// Savepoint runs fn inside a nested pgx transaction (PostgreSQL SAVEPOINT). The
// child *Session[S] receives a fresh instance-bound SessionProof and is closed
// when Savepoint returns.
func (s *Session[S]) Savepoint(ctx context.Context, fn func(context.Context, *Session[S]) error) (err error) {
	if err := s.checkContext(ctx); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("pgxdriver.Savepoint: callback is required")
	}
	beginNested := func() (pgx.Tx, error) {
		if err := s.tryLock(); err != nil {
			return nil, err
		}
		defer s.mu.Unlock()
		return s.tx.Begin(ctx)
	}
	nested, err := beginNested()
	if err != nil {
		return err
	}
	child := &Session[S]{
		mu:          s.mu,
		tx:          nested,
		scope:       s.scope,
		unscoped:    s.unscoped,
		readOnly:    s.readOnly,
		readRole:    s.readRole,
		denied:      s.denied,
		key:         s.key,
		obs:         s.obs,
		scopeLog:    s.scopeLog,
		shared:      s.shared,
		statements:  s.statements,
		authority:   s.authority,
		readQueries: readQueries{db: nested, guard: s.guard},
	}
	child.proof = s.key.Sign(child)

	release := false
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		child.closed.Store(true)
		if closed := s.checkState(); closed != nil {
			if err == nil {
				err = closed
			}
			return
		}
		s.mu.closeCursor(tabula.ErrSessionClosed)
		if release {
			err = nested.Commit(ctx)
			if err == nil {
				return
			}
		}
		_ = nested.Rollback(context.WithoutCancel(ctx))
	}()
	if err := fn(ctx, child); err != nil {
		return err
	}
	release = true
	return nil
}
