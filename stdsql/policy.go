package stdsql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xJean/tabula"
)

// UnscopedMode controls whether a Runner permits unscoped sessions.
type UnscopedMode uint8

const (
	// UnscopedDenied rejects every unscoped transaction before BEGIN (default).
	UnscopedDenied UnscopedMode = iota
	// UnscopedReadOnly permits unscoped reads in READ ONLY transactions or via
	// an explicitly configured UnscopedReadRole in WithinShared.
	UnscopedReadOnly
	// UnscopedReadWrite permits unscoped sessions in read-only and read-write transactions.
	UnscopedReadWrite
)

// Timeouts configures per-runner PostgreSQL transaction-local timeouts and an
// optional Go context deadline covering the entire attempt.
type Timeouts struct {
	// Lock maps to PostgreSQL lock_timeout via set_config(..., true).
	Lock time.Duration
	// Statement maps to PostgreSQL statement_timeout via set_config(..., true).
	Statement time.Duration
	// IdleInTransaction maps to PostgreSQL idle_in_transaction_session_timeout.
	IdleInTransaction time.Duration
	// Transaction sets a Go context deadline before BEGIN for the whole attempt.
	// Zero inherits the caller's context; an earlier caller deadline wins.
	Transaction time.Duration
}

// ScopePolicy configures transaction-local scope variables, context-derived
// variables, unscoped mode detection and SQL guards for a Runner.
type ScopePolicy[S any] struct {
	// ManagedVars declares the complete set of custom dotted GUC names managed by
	// this policy (for example "example.scope", "example.scope_mode",
	// "example.request_id"). All names are validated at construction and
	// protected automatically by the built-in session guard.
	ManagedVars []string
	// ScopeVars produces managed GUC key/value pairs for a scoped transaction.
	ScopeVars func(S) map[string]string
	// ContextVars produces managed GUC key/value pairs derived from ctx.
	ContextVars func(context.Context) map[string]string
	// IsUnscoped reports whether scope represents an unscoped call.
	IsUnscoped func(S) bool
	// UnscopedVars holds managed GUC key/value pairs applied in unscoped mode.
	UnscopedVars map[string]string
	// InactiveVars optionally specifies the inactive value written for managed
	// variables belonging to the opposite mode when switching inside WithinShared.
	// Unspecified managed variables default to "".
	InactiveVars map[string]string
	// GuardQuery optionally inspects every user SQL string before execution; it
	// composes with the runner's built-in session guard.
	GuardQuery func(query string) error
	// ScopeLog optionally formats scope values for session-level telemetry.
	ScopeLog func(S) string
}

// NoScopePolicy returns an empty ScopePolicy for applications that do not use
// row-level scoping.
func NoScopePolicy() ScopePolicy[struct{}] { return ScopePolicy[struct{}]{} }

// RunnerOptions configures a role-bound, guarded stdsql.Runner.
type RunnerOptions struct {
	// RequireCallbackContext restricts live sessions to the runner callback's
	// context and its descendants. The default accepts any context.
	RequireCallbackContext bool
	// SingleStatement restricts session SQL to one data statement before driver I/O.
	SingleStatement bool
	// Statements restricts commands by method and implies SingleStatement when configured.
	Statements tabula.StatementPolicy
	// Role selects the transaction-local PostgreSQL role during setup when non-empty.
	Role string
	// RequireRole fails NewRunner when Role is empty.
	RequireRole bool
	// ReadOnly restricts this runner to READ ONLY transactions; read-write
	// requests fail closed with tabula.ErrReadOnly before BEGIN.
	ReadOnly bool
	// Unscoped controls whether unscoped sessions are denied, read-only, or
	// read-write.
	Unscoped UnscopedMode
	// UnscopedReadRole enables read-only unscoped handles within WithinShared.
	// Requires UnscopedReadOnly and a distinct scoped Role. Migrations must grant
	// this role read access only, including through functions and inherited grants.
	// An empty value keeps unscoped access restricted to READ ONLY transactions.
	UnscopedReadRole string
	// Timeouts bounds lock waits, statement duration, idle-in-transaction duration
	// and whole-attempt Go context duration.
	Timeouts Timeouts
	// SQLAuthority verifies descriptor calls from one registry. Its DenyLegacySQL
	// policy also disables raw session methods and raw transaction access.
	SQLAuthority tabula.SQLAuthority
	// SessionKey signs every Session created by this runner for A4 sealed
	// registries.
	SessionKey tabula.SessionKey
	// ExposeTx enables WithinRaw / WithinRawOptions returning *RawSession[S]
	// with Tx() *sql.Tx access. Default false removes raw transaction escape.
	ExposeTx bool
	// Tracer, Metrics, Log, Sanitize and TagStatements configure session-level
	// observability (B1/B3) for both descriptor and hand-written SQL.
	Tracer  tabula.Tracer
	Metrics tabula.Metrics
	Log     tabula.LogConfig
	// RawErrorDiagnostics passes original errors to the statement tracer. Driver
	// messages can contain bound values. The default traces the class and SQLSTATE.
	RawErrorDiagnostics bool
	// Sanitize supplies reviewed log text for errors. The default is the error class.
	Sanitize      func(error) string
	TagStatements bool
}

// RunOptions customizes a single runner transaction invocation, including an
// optional BeforeCommit step that receives a fresh session handle after the
// body handle is closed.
type RunOptions[S any] struct {
	TxOptions    *sql.TxOptions
	BeforeCommit func(context.Context, *Session[S]) error
}

var reservedTimeoutVars = map[string]bool{
	"lock_timeout":                        true,
	"statement_timeout":                   true,
	"idle_in_transaction_session_timeout": true,
	"role":                                true,
	"session_authorization":               true,
}

func validateCustomGUC(name string) error {
	if name == "" || strings.TrimSpace(name) != name {
		return fmt.Errorf("invalid GUC name %q", name)
	}
	lower := strings.ToLower(name)
	if reservedTimeoutVars[lower] {
		return fmt.Errorf("managed GUC %q collides with reserved runner setting", name)
	}
	dot := strings.IndexByte(name, '.')
	if dot <= 0 || dot == len(name)-1 || strings.Contains(name, "..") {
		return fmt.Errorf("managed GUC %q must be a dotted custom setting (namespace.name)", name)
	}
	for i := range len(name) {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' {
			continue
		}
		return fmt.Errorf("managed GUC %q contains invalid character %q", name, c)
	}
	return nil
}

func validateTimeouts(t Timeouts) error {
	for _, item := range []struct {
		name string
		d    time.Duration
		db   bool
	}{
		{"Lock", t.Lock, true},
		{"Statement", t.Statement, true},
		{"IdleInTransaction", t.IdleInTransaction, true},
		{"Transaction", t.Transaction, false},
	} {
		if item.d < 0 {
			return fmt.Errorf("stdsql.NewRunner: negative %s timeout %v", item.name, item.d)
		}
		if item.db && item.d > 0 && item.d < time.Millisecond {
			return fmt.Errorf("stdsql.NewRunner: %s timeout %v must be at least 1ms", item.name, item.d)
		}
	}
	if t.Lock > 0 && t.Statement > 0 && t.Lock > t.Statement {
		return fmt.Errorf("stdsql.NewRunner: Lock timeout (%v) exceeds Statement timeout (%v)", t.Lock, t.Statement)
	}
	if t.Statement > 0 && t.Transaction > 0 && t.Statement > t.Transaction {
		return fmt.Errorf("stdsql.NewRunner: Statement timeout (%v) exceeds Transaction timeout (%v)", t.Statement, t.Transaction)
	}
	return nil
}

type compiledPolicy[S any] struct {
	statements   tabula.StatementGuard
	policy       ScopePolicy[S]
	managed      map[string]bool
	managedOrder []string
	inactive     map[string]string
	guard        func(string) error
}

func compilePolicy[S any](policy ScopePolicy[S], opts RunnerOptions) (compiledPolicy[S], error) {
	if opts.RequireRole && strings.TrimSpace(opts.Role) == "" {
		return compiledPolicy[S]{}, errors.New("stdsql.NewRunner: RequireRole requires a non-empty Role")
	}
	if opts.Role != "" && strings.TrimSpace(opts.Role) != opts.Role {
		return compiledPolicy[S]{}, fmt.Errorf("stdsql.NewRunner: invalid role %q", opts.Role)
	}
	if role := opts.UnscopedReadRole; role != "" {
		if opts.Unscoped != UnscopedReadOnly || opts.Role == "" || role == opts.Role {
			return compiledPolicy[S]{}, errors.New("stdsql.NewRunner: UnscopedReadRole requires UnscopedReadOnly and a distinct scoped Role")
		}
		// PostgreSQL identifiers can be truncated. Keep both role names distinct on the server.
		if strings.TrimSpace(role) != role || len(role) > 63 || len(opts.Role) > 63 ||
			strings.EqualFold(role, "none") || strings.EqualFold(opts.Role, "none") || strings.ContainsRune(role+opts.Role, 0) {
			return compiledPolicy[S]{}, errors.New("stdsql.NewRunner: invalid shared role name")
		}
	}
	if opts.Unscoped > UnscopedReadWrite {
		return compiledPolicy[S]{}, fmt.Errorf("stdsql.NewRunner: invalid Unscoped mode %d", opts.Unscoped)
	}
	if opts.ReadOnly && opts.Unscoped == UnscopedReadWrite {
		return compiledPolicy[S]{}, errors.New("stdsql.NewRunner: ReadOnly runner cannot allow UnscopedReadWrite")
	}
	if err := validateTimeouts(opts.Timeouts); err != nil {
		return compiledPolicy[S]{}, err
	}

	managed := make(map[string]bool, len(policy.ManagedVars))
	managedOrder := make([]string, 0, len(policy.ManagedVars))
	for _, v := range policy.ManagedVars {
		if err := validateCustomGUC(v); err != nil {
			return compiledPolicy[S]{}, fmt.Errorf("stdsql.NewRunner: %w", err)
		}
		if managed[v] {
			return compiledPolicy[S]{}, fmt.Errorf("stdsql.NewRunner: duplicate managed variable %q", v)
		}
		managed[v] = true
		managedOrder = append(managedOrder, v)
	}
	for k := range policy.UnscopedVars {
		if !managed[k] {
			return compiledPolicy[S]{}, fmt.Errorf("stdsql.NewRunner: UnscopedVars key %q is not declared in ManagedVars", k)
		}
	}
	inactive := make(map[string]string, len(policy.ManagedVars))
	for k, v := range policy.InactiveVars {
		if !managed[k] {
			return compiledPolicy[S]{}, fmt.Errorf("stdsql.NewRunner: InactiveVars key %q is not declared in ManagedVars", k)
		}
		inactive[k] = v
	}

	var guardPrefixes []string
	guardPrefixes = append(guardPrefixes, managedOrder...)
	hasTimeout := opts.Timeouts.Lock > 0 || opts.Timeouts.Statement > 0 || opts.Timeouts.IdleInTransaction > 0
	if opts.Timeouts.Lock > 0 {
		guardPrefixes = append(guardPrefixes, "lock_timeout")
	}
	if opts.Timeouts.Statement > 0 {
		guardPrefixes = append(guardPrefixes, "statement_timeout")
	}
	if opts.Timeouts.IdleInTransaction > 0 {
		guardPrefixes = append(guardPrefixes, "idle_in_transaction_session_timeout")
	}
	var builtInGuard func(string) error
	if opts.Role != "" || len(guardPrefixes) > 0 {
		var err error
		builtInGuard, err = tabula.NewSessionGuard(tabula.SessionGuardConfig{
			VariablePrefixes:           guardPrefixes,
			ForbidRole:                 opts.Role != "",
			ForbidSessionAuthorization: opts.Role != "",
			ForbidSetConfig:            len(managedOrder) > 0 || hasTimeout || opts.UnscopedReadRole != "",
		})
		if err != nil {
			return compiledPolicy[S]{}, fmt.Errorf("stdsql.NewRunner: session guard: %w", err)
		}
	}
	userGuard := policy.GuardQuery
	combinedGuard := composeGuards(builtInGuard, userGuard)

	statements, err := opts.Statements.Compile(opts.SingleStatement)
	if err != nil {
		return compiledPolicy[S]{}, fmt.Errorf("stdsql.NewRunner: %w", err)
	}

	cp := policy
	cp.ManagedVars = slices.Clone(managedOrder)
	cp.UnscopedVars = maps.Clone(policy.UnscopedVars)
	cp.InactiveVars = maps.Clone(inactive)
	return compiledPolicy[S]{
		statements:   statements,
		policy:       cp,
		managed:      managed,
		managedOrder: managedOrder,
		inactive:     inactive,
		guard:        combinedGuard,
	}, nil
}

func composeGuards(a, b func(string) error) func(string) error {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	default:
		return func(q string) error {
			if err := a(q); err != nil {
				return err
			}
			return b(q)
		}
	}
}

type gucPair struct {
	key string
	val string
}

func (cp compiledPolicy[S]) collectVars(ctx context.Context, scope S, unscoped, fillInactive bool) ([]gucPair, error) {
	vals := make(map[string]string, len(cp.managedOrder))
	if fillInactive {
		for _, k := range cp.managedOrder {
			vals[k] = cp.inactive[k]
		}
	}
	seenSource := make(map[string]bool, len(cp.managedOrder))
	addMap := func(src string, m map[string]string) error {
		for k, v := range m {
			if !cp.managed[k] {
				return fmt.Errorf("stdsql: %s returned undeclared GUC %q", src, k)
			}
			if seenSource[k] {
				return fmt.Errorf("stdsql: duplicate GUC %q across policy callbacks", k)
			}
			seenSource[k] = true
			vals[k] = v
		}
		return nil
	}
	if unscoped {
		if err := addMap("UnscopedVars", cp.policy.UnscopedVars); err != nil {
			return nil, err
		}
	} else if cp.policy.ScopeVars != nil {
		if err := addMap("ScopeVars", cp.policy.ScopeVars(scope)); err != nil {
			return nil, err
		}
	}
	if cp.policy.ContextVars != nil {
		if err := addMap("ContextVars", cp.policy.ContextVars(ctx)); err != nil {
			return nil, err
		}
	}
	out := make([]gucPair, 0, len(vals))
	for _, k := range cp.managedOrder {
		if v, ok := vals[k]; ok {
			out = append(out, gucPair{key: k, val: v})
		}
	}
	return out, nil
}

func timeoutPairs(t Timeouts) []gucPair {
	var out []gucPair
	if t.Lock > 0 {
		out = append(out, gucPair{"lock_timeout", strconv.FormatInt(t.Lock.Milliseconds(), 10)})
	}
	if t.Statement > 0 {
		out = append(out, gucPair{"statement_timeout", strconv.FormatInt(t.Statement.Milliseconds(), 10)})
	}
	if t.IdleInTransaction > 0 {
		out = append(out, gucPair{"idle_in_transaction_session_timeout", strconv.FormatInt(t.IdleInTransaction.Milliseconds(), 10)})
	}
	return out
}

func execSetConfigBatch(ctx context.Context, tx *sql.Tx, pairs []gucPair) error {
	if len(pairs) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("SELECT ")
	args := make([]any, 0, len(pairs)*2)
	for i, p := range pairs {
		if i > 0 {
			b.WriteString(", ")
		}
		args = append(args, p.key, p.val)
		kSlot := strconv.Itoa(len(args) - 1)
		vSlot := strconv.Itoa(len(args))
		b.WriteString("pg_catalog.set_config($" + kSlot + ", $" + vSlot + ", true)")
	}
	_, err := tx.ExecContext(ctx, b.String(), args...)
	return err
}
