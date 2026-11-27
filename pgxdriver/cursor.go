package pgxdriver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/0xJean/tabula"
)

// statementLock serializes driver calls. An open cursor reserves the transaction
// while its consumer is running, without holding the mutex across user code.
type statementLock struct {
	checkCallback func(context.Context) error
	sync.Mutex
	closed           atomic.Bool
	cursor           *rowCursor
	operation        *operationLease
	operationFailure atomic.Pointer[operationFailure]
}

type rowCursor struct {
	rows  pgx.Rows
	count int64
	done  func(int64, error)
}

func (m *statementLock) close() {
	m.Lock()
	defer m.Unlock()
	m.closed.Store(true)
	m.abandonOperations(nil)
	m.closeCursor(tabula.ErrSessionClosed)
}

// closeCursor is called under the statement lock, including at transaction and
// savepoint boundaries. A suspended consumer can resume after the connection is reused.
func (m *statementLock) closeCursor(err error) {
	if c := m.cursor; c != nil {
		m.cursor = nil
		c.rows.Close()
		c.done(c.count, err)
	}
}

// each enters and leaves with the lock held, including when decode panics.
func (m *statementLock) each(rows pgx.Rows, decode func(func(...any) error) error, done func(int64, error)) (err error) {
	c := &rowCursor{rows: rows, done: done}
	m.cursor = c
	defer func() {
		if m.cursor == c {
			m.closeCursor(err)
		}
	}()
	scan := func(dest ...any) error {
		m.Lock()
		defer m.Unlock()
		if m.cursor != c {
			return tabula.ErrSessionClosed
		}
		return rows.Scan(dest...)
	}
	for rows.Next() {
		err = func() error {
			m.Unlock()
			defer m.Lock()
			return decode(scan)
		}()
		if m.cursor != c {
			return tabula.ErrSessionClosed
		}
		if err != nil {
			return callbackReadError(rows, err)
		}
		c.count++
	}
	return directReadError(rows.Err())
}

// An unbounded exact read stops on its second row. Closing the cursor can
// receive a later PostgreSQL error for the same query.
func callbackReadError(rows pgx.Rows, err error) error {
	if errors.Is(err, tabula.ErrMultipleRows) {
		rows.Close()
		if final := rows.Err(); final != nil {
			return directReadError(final)
		}
	}
	return err
}
