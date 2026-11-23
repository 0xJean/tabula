package stdsql

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/internal/direct"
)

type readQueries struct {
	db interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	}
	guard func(string) error
}

// DirectPool owns a database/sql pool of read-only PostgreSQL connections.
type DirectPool[S any] struct {
	db   *sql.DB
	pool *direct.Pool[S]
}

// Session binds the scope without opening a transaction or checking out a connection.
func (p *DirectPool[S]) Session(scope S) tabula.Session[S] { return p.pool.Session(scope) }

// SetMaxOpenConns sets the maximum number of open connections to the database.
func (p *DirectPool[S]) SetMaxOpenConns(n int) { p.db.SetMaxOpenConns(n) }

// SetMaxIdleConns sets the maximum number of idle connections in the pool.
func (p *DirectPool[S]) SetMaxIdleConns(n int) { p.db.SetMaxIdleConns(n) }

// SetConnMaxLifetime sets the maximum amount of time a connection may be reused.
func (p *DirectPool[S]) SetConnMaxLifetime(d time.Duration) { p.db.SetConnMaxLifetime(d) }

// SetConnMaxIdleTime sets the maximum amount of time a connection may be idle.
func (p *DirectPool[S]) SetConnMaxIdleTime(d time.Duration) { p.db.SetConnMaxIdleTime(d) }

// Stats returns database/sql pool statistics.
func (p *DirectPool[S]) Stats() sql.DBStats { return p.db.Stats() }

// Close releases the underlying connection pool.
func (p *DirectPool[S]) Close() error { return p.pool.Close() }

// Direct opens a dedicated read-only pool from a PostgreSQL URL and a
// registered database/sql driver (for example "postgres"). Call Session to
// bind each request and Close at shutdown. SetupTx requires a Runner.
func Direct[S any](driverName, postgresURL string, policy ScopePolicy[S]) (*DirectPool[S], error) {
	if policy.SetupTx != nil {
		return nil, errors.New("stdsql.Direct: SetupTx requires a transaction")
	}
	u, err := url.Parse(postgresURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, errors.New("stdsql.Direct: expected a PostgreSQL URL")
	}
	params, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("stdsql.Direct: invalid connection parameters")
	}
	params.Set("default_transaction_read_only", "on")
	u.RawQuery = params.Encode()
	db, err := sql.Open(driverName, u.String())
	if err != nil {
		return nil, err
	}
	return &DirectPool[S]{
		db:   db,
		pool: direct.NewPool(readQueries{db: db, guard: direct.Guard(policy.GuardQuery)}, policy.IsUnscoped, db.Close),
	}, nil
}
