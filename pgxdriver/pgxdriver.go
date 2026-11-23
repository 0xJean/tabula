// Package pgxdriver integrates github.com/jackc/pgx/v5 with tabula:
// PostgreSQL SQLSTATE helpers, array column converters, transaction-bound
// Sessions, and pgxpool Runners with pluggable scope/RLS policies and savepoints.
package pgxdriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/0xJean/tabula"
)

// Compile-time check that *Session[S] implements tabula.Session[S].
var _ tabula.Session[struct{}] = (*Session[struct{}])(nil)

// SQLState returns the five-character PostgreSQL SQLSTATE code carried by err
// (matching *pgconn.PgError or any error implementing SQLState() string).
func SQLState(err error) string {
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pg.SQLState()
	}
	if pg, ok := errors.AsType[interface {
		error
		SQLState() string
	}](err); ok {
		return pg.SQLState()
	}
	return ""
}

// IsUniqueViolation reports SQLSTATE 23505.
func IsUniqueViolation(err error) bool { return SQLState(err) == "23505" }

// IsForeignKeyViolation reports SQLSTATE 23503.
func IsForeignKeyViolation(err error) bool { return SQLState(err) == "23503" }

// IsCheckViolation reports SQLSTATE 23514.
func IsCheckViolation(err error) bool { return SQLState(err) == "23514" }

// IsNotNullViolation reports SQLSTATE 23502.
func IsNotNullViolation(err error) bool { return SQLState(err) == "23502" }

// IsSerializationFailure reports SQLSTATE 40001.
func IsSerializationFailure(err error) bool { return SQLState(err) == "40001" }

// IsDeadlock reports SQLSTATE 40P01.
func IsDeadlock(err error) bool { return SQLState(err) == "40P01" }

// IsQueryCanceled reports SQLSTATE 57014.
func IsQueryCanceled(err error) bool { return SQLState(err) == "57014" }

// IsNumericOutOfRange reports SQLSTATE 22003.
func IsNumericOutOfRange(err error) bool { return SQLState(err) == "22003" }

// IsRejectedValue reports SQLSTATE 22001, 22003, 23502, 23514, or 22P02.
func IsRejectedValue(err error) bool {
	switch SQLState(err) {
	case "22001", "22003", "23502", "23514", "22P02":
		return true
	}
	return false
}

// ArrayColumn resolves a db:"...,array" column's Go slice type ([]~string or
// []~int64) for pgx/v5. Plain []string and []int64 pass through directly to
// pgx's native array codec; named element types convert cleanly via driver.Valuer.
func ArrayColumn(ft reflect.Type) (func(any) any, error) {
	if ft.Kind() != reflect.Slice {
		return nil, fmt.Errorf("array option requires a slice type, got %s", ft)
	}
	elem := ft.Elem()
	switch elem.Kind() {
	case reflect.String:
		if ft == reflect.TypeFor[[]string]() {
			return func(p any) any { return p }, nil
		}
		return func(p any) any { return &namedStringSlice{ptr: p, sliceType: ft, elemType: elem} }, nil
	case reflect.Int64:
		if ft == reflect.TypeFor[[]int64]() {
			return func(p any) any { return p }, nil
		}
		return func(p any) any { return &namedInt64Slice{ptr: p, sliceType: ft, elemType: elem} }, nil
	default:
		return nil, fmt.Errorf("array option supports []~string or []~int64, got %s", ft)
	}
}

type namedStringSlice struct {
	ptr       any
	sliceType reflect.Type
	elemType  reflect.Type
}

func (s *namedStringSlice) Scan(src any) error {
	rv := reflect.ValueOf(s.ptr).Elem()
	if src == nil {
		rv.Set(reflect.Zero(s.sliceType))
		return nil
	}
	switch v := src.(type) {
	case []string:
		out := reflect.MakeSlice(s.sliceType, len(v), len(v))
		for i, item := range v {
			out.Index(i).Set(reflect.ValueOf(item).Convert(s.elemType))
		}
		rv.Set(out)
		return nil
	default:
		return fmt.Errorf("pgxdriver: cannot scan %T into %s", src, s.sliceType)
	}
}

func (s *namedStringSlice) Value() (driver.Value, error) {
	rv := reflect.ValueOf(s.ptr).Elem()
	if rv.IsNil() {
		return nil, nil
	}
	out := make([]string, rv.Len())
	for i := range rv.Len() {
		out[i] = rv.Index(i).String()
	}
	return out, nil
}

type namedInt64Slice struct {
	ptr       any
	sliceType reflect.Type
	elemType  reflect.Type
}

func (s *namedInt64Slice) Scan(src any) error {
	rv := reflect.ValueOf(s.ptr).Elem()
	if src == nil {
		rv.Set(reflect.Zero(s.sliceType))
		return nil
	}
	switch v := src.(type) {
	case []int64:
		out := reflect.MakeSlice(s.sliceType, len(v), len(v))
		for i, item := range v {
			out.Index(i).Set(reflect.ValueOf(item).Convert(s.elemType))
		}
		rv.Set(out)
		return nil
	default:
		return fmt.Errorf("pgxdriver: cannot scan %T into %s", src, s.sliceType)
	}
}

func (s *namedInt64Slice) Value() (driver.Value, error) {
	rv := reflect.ValueOf(s.ptr).Elem()
	if rv.IsNil() {
		return nil, nil
	}
	out := make([]int64, rv.Len())
	for i := range rv.Len() {
		out[i] = rv.Index(i).Int()
	}
	return out, nil
}

// ScopePolicy plugs application-specific isolation rules (tenant GUC variables,
// SET LOCAL ROLE, or SQL guards) into a pgx Runner and its Sessions.
type ScopePolicy[S any] struct {
	SetupTx    func(ctx context.Context, tx pgx.Tx, scope S) error
	GuardQuery func(query string) error
	IsPlatform func(scope S) bool
}

// NoScopePolicy returns an empty ScopePolicy for applications without tenant scoping.
func NoScopePolicy() ScopePolicy[struct{}] { return ScopePolicy[struct{}]{} }

// SetLocalConfig sets a PostgreSQL transaction-local GUC via set_config(..., true).
func SetLocalConfig(ctx context.Context, tx pgx.Tx, key, value string) error {
	if key == "" {
		return errors.New("pgxdriver.SetLocalConfig: key is required")
	}
	_, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, key, value)
	return err
}

// SetLocalRole switches the PostgreSQL role for the duration of tx.
func SetLocalRole(ctx context.Context, tx pgx.Tx, role string) error {
	if role == "" {
		return errors.New("pgxdriver.SetLocalRole: role is required")
	}
	_, err := tx.Exec(ctx, `SET LOCAL ROLE `+quoteIdent(role))
	return err
}

func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// Session implements tabula.Session[S] over one pgx.Tx.
type Session[S any] struct {
	tx       pgx.Tx
	scope    S
	platform bool
	guard    func(string) error
}

// NewSession wraps an existing pgx.Tx with scope and policy.
func NewSession[S any](tx pgx.Tx, scope S, policy ScopePolicy[S]) *Session[S] {
	if tx == nil {
		panic("pgxdriver.NewSession: tx is required")
	}
	platform := false
	if policy.IsPlatform != nil {
		platform = policy.IsPlatform(scope)
	}
	return &Session[S]{
		tx:       tx,
		scope:    scope,
		platform: platform,
		guard:    policy.GuardQuery,
	}
}

// Tx returns the underlying pgx.Tx for low-level escape hatches.
func (s *Session[S]) Tx() pgx.Tx { return s.tx }

// Tenant returns the session's bound scope value.
func (s *Session[S]) Tenant() S { return s.scope }

// Platform reports whether this session runs in platform/cross-tenant mode.
func (s *Session[S]) Platform() bool { return s.platform }

func (s *Session[S]) check(query string) error {
	if s.guard != nil {
		return s.guard(query)
	}
	return nil
}

// Exec executes query and returns the affected row count.
func (s *Session[S]) Exec(ctx context.Context, _ S, query string, args ...any) (int64, error) {
	if err := s.check(query); err != nil {
		return 0, err
	}
	tag, err := s.tx.Exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Scan executes a single-row query and maps pgx.ErrNoRows to sql.ErrNoRows.
func (s *Session[S]) Scan(ctx context.Context, _ S, query string, args []any, dest ...any) error {
	if err := s.check(query); err != nil {
		return err
	}
	err := s.tx.QueryRow(ctx, query, args...).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return sql.ErrNoRows
	}
	return err
}

// Returning executes a write with RETURNING and maps pgx.ErrNoRows to sql.ErrNoRows.
func (s *Session[S]) Returning(ctx context.Context, _ S, query string, args []any, dest ...any) error {
	if err := s.check(query); err != nil {
		return err
	}
	err := s.tx.QueryRow(ctx, query, args...).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return sql.ErrNoRows
	}
	return err
}

// Each streams all rows of query through decode.
func (s *Session[S]) Each(ctx context.Context, _ S, query string, args []any, decode func(func(...any) error) error) error {
	if err := s.check(query); err != nil {
		return err
	}
	rows, err := s.tx.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := decode(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Savepoint runs fn inside a nested pgx transaction (PostgreSQL SAVEPOINT).
func (s *Session[S]) Savepoint(ctx context.Context, fn func(context.Context, *Session[S]) error) error {
	nested, err := s.tx.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = nested.Rollback(context.WithoutCancel(ctx))
		}
	}()
	child := &Session[S]{tx: nested, scope: s.scope, platform: s.platform, guard: s.guard}
	if err := fn(ctx, child); err != nil {
		return err
	}
	if err := nested.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// Runner opens scoped PostgreSQL transactions on a *pgxpool.Pool.
type Runner[S any] struct {
	pool   *pgxpool.Pool
	policy ScopePolicy[S]
}

// NewRunner creates a Runner backed by pool and policy.
func NewRunner[S any](pool *pgxpool.Pool, policy ScopePolicy[S]) *Runner[S] {
	if pool == nil {
		panic("pgxdriver.NewRunner: pool is required")
	}
	return &Runner[S]{pool: pool, policy: policy}
}

// Within runs fn in a read-write transaction.
func (r *Runner[S]) Within(ctx context.Context, scope S, fn func(context.Context, *Session[S]) error) error {
	return r.WithinTxOptions(ctx, scope, pgx.TxOptions{}, fn)
}

// WithinReadOnly runs fn in a READ ONLY transaction.
func (r *Runner[S]) WithinReadOnly(ctx context.Context, scope S, fn func(context.Context, *Session[S]) error) error {
	return r.WithinTxOptions(ctx, scope, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn)
}

// WithinTxOptions runs fn inside a transaction opened with opts.
func (r *Runner[S]) WithinTxOptions(ctx context.Context, scope S, opts pgx.TxOptions, fn func(context.Context, *Session[S]) error) error {
	tx, err := r.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("pgxdriver: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if r.policy.SetupTx != nil {
		if err := r.policy.SetupTx(ctx, tx, scope); err != nil {
			return err
		}
	}
	sess := NewSession(tx, scope, r.policy)
	if err := fn(ctx, sess); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgxdriver: commit tx: %w", err)
	}
	committed = true
	return nil
}
