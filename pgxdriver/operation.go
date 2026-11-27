package pgxdriver

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/0xJean/tabula"
)

var _ tabula.OperationSession[struct{}] = (*Session[struct{}])(nil)

type operationLease struct {
	parent *operationLease
	closed atomic.Bool
}

type operationFailure struct{ err error }

// WithinOperation runs fn in an operation savepoint. Its sealed child is the
// only usable handle until fn returns. Errors and panics roll back the operation;
// cleanup failure or cancellation discovered after a successful callback prevents
// the enclosing runner from committing.
func (s *Session[S]) WithinOperation(ctx context.Context, fn func(context.Context, tabula.Session[S]) error) (err error) {
	if s == nil {
		return tabula.ErrSessionClosed
	}
	if err := s.checkContext(ctx); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("pgxdriver: operation callback is required")
	}
	if err := s.tryLock(); err != nil {
		return err
	}
	name := "tabula_operation_" + strings.ToLower(rand.Text())
	if err := s.applyScope(ctx); err != nil {
		s.mu.Unlock()
		return err
	}
	if _, err := s.tx.Exec(ctx, "SAVEPOINT "+name); err != nil {
		s.mu.Unlock()
		return err
	}
	lease := &operationLease{parent: s.operation}
	child := s.operationChild(lease)
	childCtx, check := tabula.BindCallbackContext(ctx)
	child.checkCallback = check
	s.mu.operation = lease
	s.mu.Unlock()
	succeeded := false
	var completionErr error
	defer func() {
		recovered := recover()
		var fatal error
		if completionErr != nil || errors.Is(err, tabula.ErrUnobservedWrite) {
			fatal = err
		}
		cleanup := s.finishOperation(ctx, name, lease, succeeded, fatal)
		err = errors.Join(err, cleanup)
		if recovered != nil {
			panic(recovered)
		}
	}()
	err = fn(childCtx, child)
	if err == nil {
		completionErr = ctx.Err()
		err = completionErr
	}
	if failure := s.mu.operationError(); failure != nil {
		err = errors.Join(err, failure)
	}
	succeeded = err == nil
	return err
}

func (s *Session[S]) operationChild(lease *operationLease) *Session[S] {
	child := &Session[S]{
		authority: s.authority, statements: s.statements, mu: s.mu,
		tx: s.tx, scope: s.scope, unscoped: s.unscoped,
		readOnly: s.readOnly, readRole: s.readRole, denied: s.denied,
		key: s.key, obs: s.obs, scopeLog: s.scopeLog, shared: s.shared,
		readQueries: s.readQueries, operation: lease,
	}
	child.proof = s.key.Sign(child)
	return child
}

func (s *Session[S]) finishOperation(ctx context.Context, name string, lease *operationLease, success bool, fatal error) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lease.closed.Store(true)
	if s.mu.closed.Load() {
		return tabula.ErrSessionClosed
	}
	if s.mu.operation != lease || s.mu.cursor != nil {
		err = fmt.Errorf("pgxdriver: unfinished operation: %w", tabula.ErrSessionBusy)
		success = false
		for current := s.mu.operation; current != nil && current != lease; current = current.parent {
			current.closed.Store(true)
		}
		s.mu.closeCursor(tabula.ErrSessionClosed)
	}
	defer func() {
		s.mu.operation = lease.parent
		// Publish failure before unlocking the parent handle. The completed
		// callback may no longer have a result consumer to report it to.
		s.mu.failOperation(errors.Join(fatal, err))
	}()
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	execute := func(command string) error {
		_, failure := s.tx.Exec(cleanup, command+name)
		return failure
	}
	if success {
		if failure := s.applyScope(cleanup); failure == nil {
			failure = execute("RELEASE SAVEPOINT ")
			if failure == nil {
				return nil
			}
			err = errors.Join(err, failure)
		} else {
			err = errors.Join(err, failure)
		}
	}
	rollbackErr := execute("ROLLBACK TO SAVEPOINT ")
	releaseErr := execute("RELEASE SAVEPOINT ")
	err = errors.Join(err, rollbackErr, releaseErr)
	if err == nil {
		err = s.applyScope(cleanup)
	}
	return err
}

// WithinOperation delegates to the guarded transaction handle. The operation
// callback receives a guarded child without a raw transaction escape.
func (s *RawSession[S]) WithinOperation(ctx context.Context, fn func(context.Context, tabula.Session[S]) error) error {
	if s == nil || s.sess == nil {
		return tabula.ErrSessionClosed
	}
	return s.sess.WithinOperation(ctx, fn)
}

func (m *statementLock) failOperation(err error) {
	if err == nil {
		return
	}
	for {
		previous := m.operationFailure.Load()
		combined := err
		if previous != nil {
			if errors.Is(previous.err, err) {
				return
			}
			combined = errors.Join(err, previous.err)
		}
		if m.operationFailure.CompareAndSwap(previous, &operationFailure{combined}) {
			return
		}
	}
}

func (m *statementLock) operationError() error {
	if failure := m.operationFailure.Load(); failure != nil {
		return failure.err
	}
	return nil
}

// abandonOperations invalidates unfinished descendants at a transaction or
// public savepoint boundary. The caller holds the transaction lock.
func (m *statementLock) abandonOperations(parent *operationLease) {
	if m.operation == parent {
		return
	}
	for current := m.operation; current != nil && current != parent; current = current.parent {
		current.closed.Store(true)
	}
	m.operation = parent
	m.failOperation(fmt.Errorf("unfinished operation: %w", tabula.ErrSessionClosed))
}
