package stdsql

import (
	"context"
	"database/sql"
	"errors"
	"net/url"

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
type DirectPool[S any] struct{ pool *direct.Pool[S] }

// Session binds the scope without opening a transaction or checking out a connection.
func (p *DirectPool[S]) Session(scope S) tabula.Session[S] { return p.pool.Session(scope) }

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
	return &DirectPool[S]{pool: direct.NewPool(readQueries{db: db, guard: direct.Guard(policy.GuardQuery)}, policy.IsPlatform, db.Close)}, nil
}
