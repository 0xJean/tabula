package pgxdriver

import (
	"context"
	"errors"

	"github.com/0xJean/tabula"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type readQueries struct {
	db interface {
		QueryRow(context.Context, string, ...any) pgx.Row
		Query(context.Context, string, ...any) (pgx.Rows, error)
	}
	guard func(string) error
}

// DirectPool owns a pool of read-only pgx connections.
type DirectPool[S any] struct {
	pool   *pgxpool.Pool
	policy ScopePolicy[S]
}

// Session binds the scope without opening a transaction or checking out a connection.
func (p *DirectPool[S]) Session(scope S) tabula.Session[S] {
	return &directSession[S]{
		reader:   readQueries{db: p.pool, guard: p.check},
		scope:    scope,
		platform: p.policy.IsPlatform != nil && p.policy.IsPlatform(scope),
	}
}

func (p *DirectPool[S]) check(query string) error {
	if err := tabula.GuardReadOnly(query); err != nil {
		return err
	}
	if p.policy.GuardQuery != nil {
		return p.policy.GuardQuery(query)
	}
	return nil
}

// Close releases the underlying connection pool.
func (p *DirectPool[S]) Close() error {
	p.pool.Close()
	return nil
}

// Direct opens a dedicated pool with read-only mode in the startup parameters
// of every physical connection. Session binds each request without SQL.
// Transaction-local setup requires a Runner. Call Close at shutdown.
func Direct[S any](ctx context.Context, dsn string, policy ScopePolicy[S]) (*DirectPool[S], error) {
	if policy.SetupTx != nil {
		return nil, errors.New("pgxdriver.Direct: SetupTx requires a transaction")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &DirectPool[S]{pool: pool, policy: policy}, nil
}

type directSession[S any] struct {
	reader   readQueries
	scope    S
	platform bool
}

func (s *directSession[S]) Tenant() S      { return s.scope }
func (s *directSession[S]) Platform() bool { return s.platform }
func (s *directSession[S]) ReadOnly() bool { return true }

func (s *directSession[S]) Exec(context.Context, string, ...any) (int64, error) {
	return 0, tabula.ErrReadOnly
}

func (s *directSession[S]) Returning(context.Context, string, []any, ...any) error {
	return tabula.ErrReadOnly
}

func (s *directSession[S]) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	return directReadError(s.reader.Scan(ctx, query, args, dest...))
}

func (s *directSession[S]) Each(ctx context.Context, query string, args []any, visit func(func(...any) error) error) error {
	return directReadError(s.reader.Each(ctx, query, args, visit))
}

func directReadError(err error) error {
	if tabula.SQLState(err) == "25006" {
		return errors.Join(tabula.ErrReadOnly, err)
	}
	return err
}
