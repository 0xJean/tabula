// Package direct implements the pooled read-only sessions used by stdsql.
package direct

import (
	"context"
	"errors"

	"github.com/0xJean/tabula"
)

type Reader interface {
	Scan(context.Context, string, []any, ...any) error
	Each(context.Context, string, []any, func(func(...any) error) error) error
}

// Pool owns connections opened with default_transaction_read_only=on.
type Pool[S any] struct {
	reader    Reader
	unscoped  func(S) bool
	scopeLog  func(S) string
	authority tabula.SQLAuthority
	key       tabula.SessionKey
	obs       *tabula.Observer
	close     func() error
}

// NewPool constructs a read-only direct pool.
func NewPool[S any](r Reader, unscoped func(S) bool, closePool func() error) *Pool[S] {
	return &Pool[S]{reader: r, unscoped: unscoped, close: closePool}
}

// NewPoolWithOptions constructs a read-only direct pool with optional session
// signing key, scope logger and session-level observer.
func NewPoolWithOptions[S any](
	r Reader,
	unscoped func(S) bool,
	scopeLog func(S) string,
	key tabula.SessionKey,
	authority tabula.SQLAuthority,
	obs *tabula.Observer,
	closePool func() error,
) *Pool[S] {
	return &Pool[S]{
		reader:    r,
		unscoped:  unscoped,
		scopeLog:  scopeLog,
		authority: authority,
		key:       key,
		obs:       obs,
		close:     closePool,
	}
}

// Session binds a scope without acquiring a connection or starting a transaction.
func (p *Pool[S]) Session(scope S) tabula.Session[S] {
	s := &session[S]{
		authority: p.authority,
		reader:    p.reader,
		scope:     scope,
		unscoped:  p.unscoped != nil && p.unscoped(scope),
		scopeLog:  p.scopeLog,
		obs:       p.obs,
	}
	s.proof = p.key.Sign(s)
	return s
}

// Close releases the owned connection pool.
func (p *Pool[S]) Close() error { return p.close() }

type session[S any] struct {
	authority tabula.SQLAuthority
	reader    Reader
	scope     S
	unscoped  bool
	scopeLog  func(S) string
	obs       *tabula.Observer
	proof     tabula.SessionProof
}

func (s *session[S]) Scope() S                           { return s.scope }
func (s *session[S]) Unscoped() bool                     { return s.unscoped }
func (s *session[S]) ReadOnly() bool                     { return true }
func (s *session[S]) SessionProof() tabula.SessionProof  { return s.proof }
func (s *session[S]) TabulaObserved() tabula.ObserveMask { return s.obs.Signals() }

func (s *session[S]) scopeLabel() string {
	if s.scopeLog == nil {
		return ""
	}
	return s.scopeLog(s.scope)
}

func (s *session[S]) Exec(context.Context, string, ...any) (int64, error) {
	if s.authority.DeniesLegacySQL() {
		return 0, errors.Join(tabula.ErrReadOnly, tabula.ErrLegacySQL)
	}
	return 0, tabula.ErrReadOnly
}

func (s *session[S]) Returning(context.Context, string, []any, ...any) error {
	if s.authority.DeniesLegacySQL() {
		return errors.Join(tabula.ErrReadOnly, tabula.ErrLegacySQL)
	}
	return tabula.ErrReadOnly
}

func (s *session[S]) Scan(ctx context.Context, q string, args []any, dest ...any) error {
	if s.authority.DeniesLegacySQL() {
		return tabula.ErrLegacySQL
	}
	return s.scan(ctx, q, args, dest...)
}

func (s *session[S]) scan(ctx context.Context, q string, args []any, dest ...any) error {
	if s.obs == nil {
		return readError(s.reader.Scan(ctx, q, args, dest...))
	}
	tagged := s.obs.TagSQL(ctx, q)
	opCtx, done := s.obs.Start(ctx, s.scopeLabel(), q, args)
	err := readError(s.reader.Scan(opCtx, tagged, args, dest...))
	done(-1, err)
	return err
}

func (s *session[S]) Each(ctx context.Context, q string, args []any, visit func(func(...any) error) error) error {
	if s.authority.DeniesLegacySQL() {
		return tabula.ErrLegacySQL
	}
	return s.each(ctx, q, args, visit)
}

func (s *session[S]) each(ctx context.Context, q string, args []any, visit func(func(...any) error) error) error {
	if s.obs == nil {
		return readError(s.reader.Each(ctx, q, args, visit))
	}
	tagged := s.obs.TagSQL(ctx, q)
	opCtx, done := s.obs.Start(ctx, s.scopeLabel(), q, args)
	var count int64
	err := readError(s.reader.Each(opCtx, tagged, args, func(scan func(...any) error) error {
		count++
		return visit(scan)
	}))
	done(count, err)
	return err
}

func readError(err error) error {
	if tabula.SQLState(err) == "25006" {
		return errors.Join(tabula.ErrReadOnly, err)
	}
	return err
}

// Guard applies the Direct query restrictions before the application's policy.
func Guard(policy func(string) error) func(string) error {
	if policy == nil {
		return tabula.GuardReadOnly
	}
	return func(query string) error {
		if err := tabula.GuardReadOnly(query); err != nil {
			return err
		}
		return policy(query)
	}
}
