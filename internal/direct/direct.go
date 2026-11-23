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
	reader   Reader
	unscoped func(S) bool
	close    func() error
}

func NewPool[S any](r Reader, unscoped func(S) bool, close func() error) *Pool[S] {
	return &Pool[S]{reader: r, unscoped: unscoped, close: close}
}

// Session binds a scope without acquiring a connection or starting a transaction.
func (p *Pool[S]) Session(scope S) tabula.Session[S] {
	return &session[S]{reader: p.reader, scope: scope, unscoped: p.unscoped != nil && p.unscoped(scope)}
}

// Close releases the owned connection pool.
func (p *Pool[S]) Close() error { return p.close() }

type session[S any] struct {
	reader   Reader
	scope    S
	unscoped bool
}

func (s *session[S]) Scope() S       { return s.scope }
func (s *session[S]) Unscoped() bool { return s.unscoped }
func (s *session[S]) ReadOnly() bool { return true }
func (s *session[S]) Exec(context.Context, string, ...any) (int64, error) {
	return 0, tabula.ErrReadOnly
}

func (s *session[S]) Returning(context.Context, string, []any, ...any) error {
	return tabula.ErrReadOnly
}

func (s *session[S]) Scan(ctx context.Context, q string, args []any, dest ...any) error {
	return readError(s.reader.Scan(ctx, q, args, dest...))
}

func (s *session[S]) Each(ctx context.Context, q string, args []any, visit func(func(...any) error) error) error {
	return readError(s.reader.Each(ctx, q, args, visit))
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
