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
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"
)

type staticError string

func (e staticError) Error() string { return string(e) }

const (
	// Default sentinel errors returned when ErrorPolicy leaves a slot nil.
	ErrConflict   staticError = "row conflict or lost update"
	ErrOnce       staticError = "insert-only column changed on update"
	ErrNotDeleted staticError = "soft delete needs the deleted column set"
	ErrRowCount   staticError = "write did not confirm exactly one row"
)

// Session is the narrow transaction-bound executor a Rows handle drives.
// S is the host application's scope value (for example a tenant scope, or
// struct{} for applications without row-level tenant scoping).
type Session[S any] interface {
	Tenant() S
	Platform() bool
	Exec(ctx context.Context, scope S, query string, args ...any) (int64, error)
	Scan(ctx context.Context, scope S, query string, args []any, dest ...any) error
	Returning(ctx context.Context, scope S, query string, args []any, dest ...any) error
	Each(ctx context.Context, scope S, query string, args []any, decode func(func(...any) error) error) error
}

// ErrorPolicy maps execution and write-guard failures into the host's own
// error categories. Nil fields fall back to the package's default sentinels.
type ErrorPolicy struct {
	Classify    func(ctx context.Context, err error) error
	Conflict    error
	OnceChanged error
	NotDeleted  error
	RowCount    error
}

// RegistryConfig configures one isolated Registry for scope type S.
type RegistryConfig[S any] struct {
	// Prefix labels panic and declaration error messages (defaults to "tabula").
	Prefix string
	// TenantID extracts the raw SQL bind argument from a scope value S.
	// Required when a table declares a ",tenant" column.
	TenantID func(S) any
	// TenantWrap wraps a *S pointer into a sql.Scanner / driver.Valuer.
	// Optional when S already implements sql.Scanner and driver.Valuer.
	TenantWrap func(*S) any
	// ArrayArg wraps a Go slice for PostgreSQL "= ANY($n)" parameters
	// (e.g. pqdriver.Array). When nil, slices are passed as-is (native pgx).
	ArrayArg func(any) any
	// ArrayColumn resolves a db:"...,array" column's Go slice type into a
	// converter (e.g. pqdriver.ArrayColumn).
	ArrayColumn func(reflect.Type) (func(any) any, error)
	// Errors maps driver and write-guard failures for Rows created from this
	// registry.
	Errors ErrorPolicy
}

// Registry owns registered value converters, error policy, and the list of
// defined tables and views. Applications normally declare one package-level
// Registry and may construct isolated registries with NewRegistry in tests.
type Registry[S any] struct {
	prefix     string
	tenantType reflect.Type
	tenantID   func(S) any
	arrayArgFn func(any) any
	arrayColFn func(reflect.Type) (func(any) any, error)
	errors     ErrorPolicy

	mu    sync.RWMutex
	types map[reflect.Type]func(any) any
	zeros map[reflect.Type]func(any) any
	defs  []Definition
	views []ViewDefinition
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
		tenantType: reflect.TypeFor[S](),
		tenantID:   cfg.TenantID,
		arrayArgFn: cfg.ArrayArg,
		arrayColFn: cfg.ArrayColumn,
		errors:     cfg.Errors,
		types:      map[reflect.Type]func(any) any{},
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
	if cfg.TenantWrap != nil {
		r.types[r.tenantType] = func(p any) any { return cfg.TenantWrap(p.(*S)) }
	}
	return r
}

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
	out := make([]Definition, len(r.defs))
	for i, d := range r.defs {
		out[i] = cloneDefinition(d)
	}
	return out
}

func (r *Registry[S]) record(name string, cols []DefinedColumn, stmts map[string]string, privs []DefinedPrivilege) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defs = append(r.defs, Definition{Name: name, Columns: cols, Statements: stmts, Privileges: privs})
	return len(r.defs)
}

// DefinedViews lists every read-only View defined in r in definition order.
func (r *Registry[S]) DefinedViews() []ViewDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ViewDefinition, len(r.views))
	for i, v := range r.views {
		out[i] = cloneViewDefinition(v)
	}
	return out
}

func (r *Registry[S]) recordView(name string, cols []DefinedColumn) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.views = append(r.views, ViewDefinition{Name: name, Columns: cols, Statements: map[string]string{}})
	return len(r.views)
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
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func (r *Registry[S]) tenantValue(s S) any {
	if r.tenantID != nil {
		return r.tenantID(s)
	}
	return s
}

func cloneDefinition(d Definition) Definition {
	return Definition{
		Name:       d.Name,
		Columns:    slices.Clone(d.Columns),
		Statements: maps.Clone(d.Statements),
		Privileges: slices.Clone(d.Privileges),
	}
}
