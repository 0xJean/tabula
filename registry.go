// Package tabula is a zero-dependency, descriptor-driven single-table SQL
// engine for PostgreSQL. A row struct carries lightweight `db` tags; Define
// validates the declaration once at package initialization, derives static
// parameterized statements and column privilege needs, and records them in a
// Registry for verification (PREPARE/EXPLAIN, schema checks, SQL snapshots).
//
// Host applications inject their own scope type S (e.g. a tenant scope or
// struct{}), value converters, and error classification via RegistryConfig so
// this package stays decoupled from any application domain or error taxonomy.
package tabula

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"
)

// Session executes SQL under a single fixed scope. Transaction adapters and
// Direct pools implement this interface. Each must close its cursor and stop
// on the first callback error. A session may implement ReadOnly() bool to
// decline derived writes and locking reads before any SQL is sent.
// S is the host application's row-scope value (for example a tenant, org, or
// workspace ID, or struct{} for applications without row-level scoping).
type Session[S any] interface {
	Scope() S
	Unscoped() bool
	Exec(ctx context.Context, query string, args ...any) (int64, error)
	Scan(ctx context.Context, query string, args []any, dest ...any) error
	Returning(ctx context.Context, query string, args []any, dest ...any) error
	Each(ctx context.Context, query string, args []any, decode func(func(...any) error) error) error
}

// ErrorPolicy maps execution and write-guard failures into the host's own
// error categories. Nil fields fall back to the package's default sentinels.
type ErrorPolicy struct {
	Classify    func(ctx context.Context, err error) error
	Sanitize    func(err error) string
	Conflict    error
	OnceChanged error
	NotDeleted  error
	RowCount    error
}

// RegistryConfig configures one isolated Registry for scope type S.
type RegistryConfig[S any] struct {
	// Prefix labels panic and declaration error messages (defaults to "tabula").
	Prefix string
	// ScopeID extracts the raw SQL bind argument from a scope value S.
	// When omitted, the scope value itself identifies the row scope.
	ScopeID func(S) any
	// ScopeWrap wraps a *S pointer into a sql.Scanner / driver.Valuer.
	// Optional when S already implements sql.Scanner and driver.Valuer.
	ScopeWrap func(*S) any
	// ScopeLog formats a scope value S into the "tabula.scope" log attribute.
	// When nil, "tabula.scope" is omitted from log entries.
	ScopeLog func(S) string
	// ArrayArg wraps a Go slice for PostgreSQL "= ANY($n)" parameters
	// (e.g. pqdriver.Array). When nil, slices are passed as-is (native pgx).
	ArrayArg func(any) any
	// ArrayColumn resolves a db:"...,array" column's Go slice type into a
	// converter (e.g. pqdriver.ArrayColumn).
	ArrayColumn func(reflect.Type) (func(any) any, error)
	// Errors maps driver and write-guard failures for Rows created from this
	// registry.
	Errors ErrorPolicy
	// Log configures opt-in structured statement logging via log/slog.
	Log LogConfig
	// Tracer optionally wraps every statement execution in a tracing span.
	Tracer Tracer
	// Metrics optionally records latency, row count, and error class per statement.
	Metrics Metrics
}

// Registry owns registered value converters, error policy, logging, tracing,
// metrics, and the list of defined tables and views. Applications normally
// declare one package-level Registry and may construct isolated registries in tests.
type Registry[S any] struct {
	prefix     string
	scopeType  reflect.Type
	scopeID    func(S) any
	scopeLog   func(S) string
	arrayArgFn func(any) any
	arrayColFn func(reflect.Type) (func(any) any, error)
	errors     ErrorPolicy
	log        *logState
	tracer     Tracer
	metrics    Metrics

	mu    sync.RWMutex
	types map[reflect.Type]func(any) any
	zeros map[reflect.Type]func(any) any
	defs  []*registration
	views []*registration
}

// NewRegistry creates a Registry preloaded with built-in scalar zero
// converters (string, bool, int, int32, int64, float64, time.Time).
func NewRegistry[S any](cfg RegistryConfig[S]) *Registry[S] {
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = "tabula"
	}
	r := &Registry[S]{
		prefix:     prefix,
		scopeType:  reflect.TypeFor[S](),
		scopeID:    cfg.ScopeID,
		scopeLog:   cfg.ScopeLog,
		arrayArgFn: cfg.ArrayArg,
		arrayColFn: cfg.ArrayColumn,
		errors:     cfg.Errors,
		log:        newLogState(cfg.Log),
		tracer:     cfg.Tracer,
		metrics:    cfg.Metrics,
		types:      networkTypes(),
		zeros: map[reflect.Type]func(any) any{
			reflect.TypeFor[string]():    zeroOf[string],
			reflect.TypeFor[bool]():      zeroOf[bool],
			reflect.TypeFor[int]():       zeroOf[int],
			reflect.TypeFor[int32]():     zeroOf[int32],
			reflect.TypeFor[int64]():     zeroOf[int64],
			reflect.TypeFor[float64]():   zeroOf[float64],
			reflect.TypeFor[time.Time](): zeroOf[time.Time],
		},
	}
	if cfg.ScopeWrap != nil {
		r.types[r.scopeType] = func(p any) any { return cfg.ScopeWrap(p.(*S)) }
	}
	return r
}

// SetLogLevel atomically updates this Registry's statement logging level.
func (r *Registry[S]) SetLogLevel(level LogLevel) { r.log.level.Set(level) }

// LogLevel returns this Registry's active statement logging level.
func (r *Registry[S]) LogLevel() LogLevel { return r.log.level.Level() }

// RegisterType registers a converter wrapping a pointer to a value of type T.
// Call during initialization before Define.
func RegisterType[S, V any](r *Registry[S], wrap func(*V) any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.types[reflect.TypeFor[V]()] = func(p any) any { return wrap(p.(*V)) }
}

// RegisterZero registers scalar type V for the ",zero" db tag option (NULL
// scans as V's zero value; writes bind V directly).
func RegisterZero[S, V any](r *Registry[S]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.zeros[reflect.TypeFor[V]()] = zeroOf[V]
}

// Defined lists every table defined in r in definition order. The returned
// definitions are deep copies.
func (r *Registry[S]) Defined() []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return snapshots[Definition](r.defs)
}

// DefinedViews returns snapshots of all registered views in definition order.
func (r *Registry[S]) DefinedViews() []ViewDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return snapshots[ViewDefinition](r.views)
}

func snapshots[D Definition | ViewDefinition](entries []*registration) []D {
	out := make([]D, len(entries))
	for i, e := range entries {
		out[i] = D(cloneDefinition(e.def))
	}
	return out
}

// registration belongs to the Registry. All accesses use the registry mutex.
type registration struct {
	def   Definition
	bySQL map[string]stmtMeta
}

func (r *Registry[S]) record(def Definition, view bool) *registration {
	e := &registration{def: def, bySQL: make(map[string]stmtMeta)}
	r.mu.Lock()
	defer r.mu.Unlock()
	if view {
		r.views = append(r.views, e)
	} else {
		r.defs = append(r.defs, e)
	}
	return e
}

func (r *Registry[S]) arrayArg(slice any) any {
	if r.arrayArgFn != nil {
		return r.arrayArgFn(slice)
	}
	return slice
}

func (r *Registry[S]) conflictErr() error {
	if r.errors.Conflict != nil {
		return r.errors.Conflict
	}
	return ErrConflict
}

func (r *Registry[S]) onceErr() error {
	if r.errors.OnceChanged != nil {
		return r.errors.OnceChanged
	}
	return ErrOnce
}

func (r *Registry[S]) notDeletedErr() error {
	if r.errors.NotDeleted != nil {
		return r.errors.NotDeleted
	}
	return ErrNotDeleted
}

func (r *Registry[S]) rowCountErr() error {
	if r.errors.RowCount != nil {
		return r.errors.RowCount
	}
	return ErrRowCount
}

func (r *Registry[S]) classifyErr(ctx context.Context, err error) error {
	if r.errors.Classify != nil {
		return r.errors.Classify(ctx, err)
	}
	_, sentinel := ClassifyError(ctx, err)
	if errors.Is(sentinel, ErrCanceled) {
		if ctx != nil && ctx.Err() != nil {
			return errors.Join(ErrCanceled, ctx.Err())
		}
		return errors.Join(ErrCanceled, err)
	}
	if sentinel != nil {
		return errors.Join(sentinel, err)
	}
	return err
}

func (r *Registry[S]) scopeValue(s S) any {
	if r.scopeID != nil {
		return r.scopeID(s)
	}
	return s
}

func cloneDefinition(d Definition) Definition {
	privs := slices.Clone(d.Privileges)
	for i := range privs {
		privs[i].Columns = slices.Clone(privs[i].Columns)
	}
	return Definition{
		Name:       d.Name,
		Columns:    slices.Clone(d.Columns),
		Statements: maps.Clone(d.Statements),
		Privileges: privs,
	}
}
