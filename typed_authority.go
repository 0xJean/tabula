package tabula

import (
	"context"
	"slices"
)

// SQLAuthority verifies registered statements for a driver on behalf of one
// registry. It cannot sign SQL or change which statements that registry owns.
type SQLAuthority struct{ issuer *statementIssuer }

type statementIssuer struct{ _ byte }

// SQLAuthority returns this registry's verification credential. Pass it to a
// driver that executes authorized statements for this registry.
func (r *Registry[S]) SQLAuthority() SQLAuthority { return SQLAuthority{r.statementIssuer} }

// IsZero reports whether this is the unset credential.
func (a SQLAuthority) IsZero() bool { return a.issuer == nil }

// ErrStatementAuthority reports an unregistered call, a different registry's
// credential, or a call delivered to a different session.
const ErrStatementAuthority staticError = "tabula: statement authority or session does not match"

// CallMethod is the driver operation authorized for one execution.
type CallMethod uint8

const (
	// CallExec executes an affected-row mutation without a result shape.
	CallExec CallMethod = iota + 1
	// CallScan decodes a single read result.
	CallScan
	// CallEach visits results of a read.
	CallEach
	// CallReturning decodes a single row produced by a write.
	CallReturning
	// CallReturningEach streams rows produced by a write.
	CallReturningEach
)

func (s *compiledShape) permitsMethod(method CallMethod) bool {
	write := s.shape.Effects&EffectWrite != 0
	rows := len(s.shape.Result) != 0
	switch method {
	case CallExec:
		return write && !rows
	case CallScan, CallEach:
		return !write && rows
	case CallReturning, CallReturningEach:
		return write && rows
	default:
		return false
	}
}

// AuthorizedStatement is an opaque call bound to one registered shape, one
// session instance and its ordered parameters. Applications cannot construct it
// from SQL text, a snapshot or a verification credential.
type AuthorizedStatement struct {
	method CallMethod
	shape  *compiledShape
	owner  any
	args   []any
}

// AuthorizedSQL is the driver view of a successfully verified call. Its
// fields describe this call only. Editing the view cannot authorize SQL.
type AuthorizedSQL struct {
	Query, Key string
	Args       []any
	Effects    ShapeEffects
	// SecretArgs uses the same bind order as Args.
	SecretArgs []bool
}

// Open validates a compiled call for the receiving session. Drivers pass their
// own session pointer. The returned parameters are in PostgreSQL bind order.
func (a SQLAuthority) Open(owner any, method CallMethod, call AuthorizedStatement) (AuthorizedSQL, error) {
	if a.IsZero() || method != call.method || call.shape == nil || call.shape.authority != a.issuer || !eligibleOwner(owner) || call.owner != owner {
		return AuthorizedSQL{}, ErrStatementAuthority
	}
	s := call.shape
	secret := make([]bool, len(s.shape.Inputs))
	for i, input := range s.shape.Inputs {
		secret[i] = input.Secret
	}
	return AuthorizedSQL{Query: s.sql, Key: s.observationKey, Args: slices.Clone(call.args), Effects: s.shape.Effects, SecretArgs: secret}, nil
}

// AuthorizedSession is the driver channel used for registered queries. Every
// entry verifies its AuthorizedStatement against the receiving session and
// applies the usual method, transaction and database guards.
type AuthorizedSession interface {
	ExecAuthorized(context.Context, AuthorizedStatement) (int64, error)
	ScanAuthorized(context.Context, AuthorizedStatement, ...any) error
	EachAuthorized(context.Context, AuthorizedStatement, func(func(...any) error) error) error
}

// bindCall is the only constructor for driver calls. Validate the registry's
// session proof before consulting session scope or invoking the driver.
func (r *Registry[S]) bindCall(shape *compiledShape, s Session[S], method CallMethod, values []Binding) (AuthorizedStatement, error) {
	if err := r.checkSession(s); err != nil {
		return AuthorizedStatement{}, err
	}
	if shape == nil || shape.authority == nil || shape.authority != r.statementIssuer || !eligibleOwner(s) {
		return AuthorizedStatement{}, ErrStatementAuthority
	}
	if !shape.permitsMethod(method) {
		return AuthorizedStatement{}, ErrStatementAuthority
	}
	if shape.shape.UnscopedOnly && !s.Unscoped() {
		return AuthorizedStatement{}, ErrStatementAuthority
	}
	effects := shape.shape.Effects
	if effects&(EffectWrite|EffectRowLock) != 0 {
		if ro, ok := s.(interface{ ReadOnly() bool }); ok && ro.ReadOnly() {
			return AuthorizedStatement{}, ErrReadOnly
		}
	}
	if effects&EffectWrite != 0 && r.deny&DenyUnscopedWrites != 0 && s.Unscoped() {
		return AuthorizedStatement{}, ErrCapabilityDenied
	}
	var scope any
	if shape.layout.scoped {
		if s.Unscoped() {
			return AuthorizedStatement{}, ErrStatementAuthority
		}
		scope = r.scopeValue(s.Scope())
	}
	args, err := shape.layout.bind(scope, values)
	if err != nil {
		return AuthorizedStatement{}, err
	}
	return AuthorizedStatement{method, shape, s, args}, nil
}
