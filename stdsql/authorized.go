package stdsql

import (
	"context"

	"github.com/0xJean/tabula"
)

var (
	_ tabula.AuthorizedSession = (*Session[struct{}])(nil)
	_ tabula.AuthorizedSession = (*RawSession[struct{}])(nil)
)

// SQLAuthority returns the registry credential configured on the runner.
func (s *Session[S]) SQLAuthority() tabula.SQLAuthority { return s.authority }

// SQLAuthority returns the credential of the underlying session.
func (s *RawSession[S]) SQLAuthority() tabula.SQLAuthority {
	if s.sess == nil {
		return tabula.SQLAuthority{}
	}
	return s.sess.authority
}

func (s *Session[S]) allowText(ctx context.Context) error {
	if !s.authority.DeniesLegacySQL() {
		return nil
	}
	if err := s.checkContext(ctx); err != nil {
		return err
	}
	if err := s.checkState(); err != nil {
		return err
	}
	return tabula.ErrLegacySQL
}

// Exec executes SQL text on a runner that allows it.
func (s *Session[S]) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	if err := s.allowText(ctx); err != nil {
		return 0, err
	}
	return s.exec(ctx, query, args)
}

// Scan reads a single row on a runner that allows SQL text.
func (s *Session[S]) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	if err := s.allowText(ctx); err != nil {
		return err
	}
	return s.scan(ctx, "Scan", query, args, dest...)
}

// Returning reads one row from a write on a runner that allows SQL text.
func (s *Session[S]) Returning(ctx context.Context, query string, args []any, dest ...any) error {
	if err := s.allowText(ctx); err != nil {
		return err
	}
	return s.returning(ctx, query, args, dest...)
}

// Each streams rows on a runner that allows SQL text. The runner closes an
// unfinished cursor at transaction or savepoint exit.
func (s *Session[S]) Each(ctx context.Context, query string, args []any, visit func(func(...any) error) error) error {
	if err := s.allowText(ctx); err != nil {
		return err
	}
	return s.each(ctx, "Each", query, args, visit)
}

func (s *Session[S]) openAuthorized(ctx context.Context, owner any, method tabula.CallMethod, call tabula.AuthorizedStatement) (tabula.AuthorizedSQL, error) {
	if err := s.checkContext(ctx); err != nil {
		return tabula.AuthorizedSQL{}, err
	}
	if err := s.checkState(); err != nil {
		return tabula.AuthorizedSQL{}, err
	}
	q, err := s.authority.Open(owner, method, call)
	if err != nil {
		return tabula.AuthorizedSQL{}, err
	}
	if s.readOnly && q.Effects&(tabula.EffectWrite|tabula.EffectRowLock) != 0 {
		return tabula.AuthorizedSQL{}, tabula.ErrReadOnly
	}

	return q, nil
}

// ExecAuthorized executes a registered mutation and returns its affected-row count.
func (s *Session[S]) ExecAuthorized(ctx context.Context, call tabula.AuthorizedStatement) (int64, error) {
	return s.execAuthorized(ctx, s, call)
}

func (s *Session[S]) execAuthorized(ctx context.Context, owner any, call tabula.AuthorizedStatement) (int64, error) {
	q, err := s.openAuthorized(ctx, owner, tabula.CallExec, call)
	if err != nil {
		return 0, err
	}
	return s.exec(q.ObservationContext(ctx), q.Query, q.Args)
}

// ScanAuthorized decodes one result of a registered read or write. A write
// follows the runner's Returning policy.
func (s *Session[S]) ScanAuthorized(ctx context.Context, call tabula.AuthorizedStatement, dest ...any) error {
	return s.scanAuthorized(ctx, s, call, dest...)
}

func (s *Session[S]) scanAuthorized(ctx context.Context, owner any, call tabula.AuthorizedStatement, dest ...any) error {
	method, policy := tabula.CallScan, "Scan"
	if call.Method() == tabula.CallReturning {
		method, policy = tabula.CallReturning, "Returning"
	}
	q, err := s.openAuthorized(ctx, owner, method, call)
	if err != nil {
		return err
	}
	return s.scan(q.ObservationContext(ctx), policy, q.Query, q.Args, dest...)
}

// EachAuthorized streams a registered read or multirow write. A returning
// write uses the write policy and holds the cursor until iteration ends.
func (s *Session[S]) EachAuthorized(ctx context.Context, call tabula.AuthorizedStatement, visit func(func(...any) error) error) error {
	return s.eachAuthorized(ctx, s, call, visit)
}

func (s *Session[S]) eachAuthorized(ctx context.Context, owner any, call tabula.AuthorizedStatement, visit func(func(...any) error) error) error {
	method, policy := tabula.CallEach, "Each"
	if call.Method() == tabula.CallReturningEach {
		method, policy = tabula.CallReturningEach, "Returning"
	}
	q, err := s.openAuthorized(ctx, owner, method, call)
	if err != nil {
		return err
	}
	return s.each(q.ObservationContext(ctx), policy, q.Query, q.Args, visit)
}

// ExecAuthorized verifies a registered mutation for this raw-session instance.
func (s *RawSession[S]) ExecAuthorized(ctx context.Context, call tabula.AuthorizedStatement) (int64, error) {
	return s.sess.execAuthorized(ctx, s, call)
}

// ScanAuthorized verifies a registered single-row call for this instance.
func (s *RawSession[S]) ScanAuthorized(ctx context.Context, call tabula.AuthorizedStatement, dest ...any) error {
	return s.sess.scanAuthorized(ctx, s, call, dest...)
}

// EachAuthorized verifies a registered streaming call for this instance.
func (s *RawSession[S]) EachAuthorized(ctx context.Context, call tabula.AuthorizedStatement, visit func(func(...any) error) error) error {
	return s.sess.eachAuthorized(ctx, s, call, visit)
}
