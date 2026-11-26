package direct

import (
	"context"

	"github.com/0xJean/tabula"
)

var _ tabula.AuthorizedSession = (*session[struct{}])(nil)

func (s *session[S]) SQLAuthority() tabula.SQLAuthority { return s.authority }

func (s *session[S]) openRead(method tabula.CallMethod, call tabula.AuthorizedStatement) (tabula.AuthorizedSQL, error) {
	q, err := s.authority.Open(s, method, call)
	if err != nil {
		return tabula.AuthorizedSQL{}, err
	}
	if q.Effects&(tabula.EffectWrite|tabula.EffectRowLock|tabula.EffectAdvisoryLock) != 0 {
		return tabula.AuthorizedSQL{}, tabula.ErrReadOnly
	}

	return q, nil
}

func (s *session[S]) ExecAuthorized(_ context.Context, call tabula.AuthorizedStatement) (int64, error) {
	if _, err := s.openRead(tabula.CallExec, call); err != nil {
		return 0, err
	}
	return 0, tabula.ErrReadOnly
}

func (s *session[S]) ScanAuthorized(ctx context.Context, call tabula.AuthorizedStatement, dest ...any) error {
	method := tabula.CallScan
	if call.Method() == tabula.CallReturning {
		method = tabula.CallReturning
	}
	q, err := s.openRead(method, call)
	if err != nil {
		return err
	}
	return s.scan(q.ObservationContext(ctx), q.Query, q.Args, dest...)
}

func (s *session[S]) EachAuthorized(ctx context.Context, call tabula.AuthorizedStatement, visit func(func(...any) error) error) error {
	method := tabula.CallEach
	if call.Method() == tabula.CallReturningEach {
		method = tabula.CallReturningEach
	}
	q, err := s.openRead(method, call)
	if err != nil {
		return err
	}
	return s.each(q.ObservationContext(ctx), q.Query, q.Args, visit)
}
