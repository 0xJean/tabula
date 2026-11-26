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

// DirectOption customizes the pgxpool.Config used by Direct or DirectWithConfig
// before the pool is created. Direct always enforces
// default_transaction_read_only=on after applying options.
type DirectOption func(*pgxpool.Config)

// DirectConfig configures A4 session signing and B1/B3 observability on a
// read-only DirectPool created by DirectWithConfig.
type DirectConfig struct {
	// SQLAuthority accepts calls issued by one registry. A strict credential
	// also rejects direct SQL text on sessions of this pool.
	SQLAuthority tabula.SQLAuthority
	SessionKey   tabula.SessionKey
	Observer     *tabula.Observer
}

// DirectPool owns a pool of read-only pgx connections.
type DirectPool[S any] struct {
	authority tabula.SQLAuthority
	pool      *pgxpool.Pool
	policy    ScopePolicy[S]
	key       tabula.SessionKey
	obs       *tabula.Observer
}

// Session binds the scope without opening a transaction or checking out a connection.
func (p *DirectPool[S]) Session(scope S) tabula.Session[S] {
	s := &directSession[S]{
		authority: p.authority,
		reader:    readQueries{db: p.pool, guard: p.check},
		scope:     scope,
		unscoped:  p.policy.IsUnscoped != nil && p.policy.IsUnscoped(scope),
		scopeLog:  p.policy.ScopeLog,
		obs:       p.obs,
	}
	s.proof = p.key.Sign(s)
	return s
}

// Stat returns the current pgxpool statistics.
func (p *DirectPool[S]) Stat() *pgxpool.Stat { return p.pool.Stat() }

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
// Managed transaction-local variables require a Runner. Call Close at shutdown.
func Direct[S any](ctx context.Context, dsn string, policy ScopePolicy[S], opts ...DirectOption) (*DirectPool[S], error) {
	return DirectWithConfig(ctx, dsn, policy, DirectConfig{}, opts...)
}

// DirectWithConfig is Direct with A4 SessionKey signing and optional B1/B3
// session observability while preserving native DirectOption hooks and
// default_transaction_read_only=on.
func DirectWithConfig[S any](ctx context.Context, dsn string, policy ScopePolicy[S], dcfg DirectConfig, opts ...DirectOption) (*DirectPool[S], error) {
	if len(policy.ManagedVars) > 0 || policy.ScopeVars != nil || policy.ContextVars != nil || len(policy.UnscopedVars) > 0 {
		return nil, errors.New("pgxdriver.Direct: transaction-local scope variables require a Runner")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = make(map[string]string)
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &DirectPool[S]{pool: pool, policy: policy, authority: dcfg.SQLAuthority, key: dcfg.SessionKey, obs: dcfg.Observer}, nil
}

type directSession[S any] struct {
	authority tabula.SQLAuthority
	reader    readQueries
	scope     S
	unscoped  bool
	scopeLog  func(S) string
	proof     tabula.SessionProof
	obs       *tabula.Observer
}

func (s *directSession[S]) Scope() S                           { return s.scope }
func (s *directSession[S]) Unscoped() bool                     { return s.unscoped }
func (s *directSession[S]) ReadOnly() bool                     { return true }
func (s *directSession[S]) SessionProof() tabula.SessionProof  { return s.proof }
func (s *directSession[S]) TabulaObserved() tabula.ObserveMask { return s.obs.Signals() }

func (s *directSession[S]) scopeLabel() string {
	if s.scopeLog == nil {
		return ""
	}
	return s.scopeLog(s.scope)
}

func (s *directSession[S]) Exec(context.Context, string, ...any) (int64, error) {
	if s.authority.DeniesLegacySQL() {
		return 0, errors.Join(tabula.ErrReadOnly, tabula.ErrLegacySQL)
	}
	return 0, tabula.ErrReadOnly
}

func (s *directSession[S]) Returning(context.Context, string, []any, ...any) error {
	if s.authority.DeniesLegacySQL() {
		return errors.Join(tabula.ErrReadOnly, tabula.ErrLegacySQL)
	}
	return tabula.ErrReadOnly
}

func (s *directSession[S]) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	if s.authority.DeniesLegacySQL() {
		return tabula.ErrLegacySQL
	}
	return s.scan(ctx, query, args, dest...)
}

func (s *directSession[S]) scan(ctx context.Context, query string, args []any, dest ...any) error {
	if err := s.reader.check(query); err != nil {
		return err
	}
	tagged := s.obs.TagSQL(ctx, query)
	opCtx, done := s.obs.Start(ctx, s.scopeLabel(), query, args)
	err := directReadError(s.reader.Scan(opCtx, tagged, args, dest...))
	done(-1, err)
	return err
}

func (s *directSession[S]) Each(ctx context.Context, query string, args []any, visit func(func(...any) error) error) error {
	if s.authority.DeniesLegacySQL() {
		return tabula.ErrLegacySQL
	}
	return s.each(ctx, query, args, visit)
}

func (s *directSession[S]) each(ctx context.Context, query string, args []any, visit func(func(...any) error) error) error {
	if err := s.reader.check(query); err != nil {
		return err
	}
	tagged := s.obs.TagSQL(ctx, query)
	opCtx, done := s.obs.Start(ctx, s.scopeLabel(), query, args)
	var count int64
	err := directReadError(s.reader.Each(opCtx, tagged, args, func(scan func(...any) error) error {
		if err := visit(scan); err != nil {
			return err
		}
		count++
		return nil
	}))
	done(count, err)
	return err
}

func directReadError(err error) error {
	if tabula.SQLState(err) == "25006" {
		return errors.Join(tabula.ErrReadOnly, err)
	}
	return err
}
