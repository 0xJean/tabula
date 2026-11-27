package tabula

import (
	"context"
	"fmt"
	"slices"
)

// SQLAuthority verifies registered statements for a driver on behalf of one
// registry. It cannot sign SQL or change which statements that registry owns.
type SQLAuthority struct{ issuer *statementIssuer }

type statementIssuer struct{ strict bool }

// SQLAuthority returns this registry's verification credential. Pass it to a
// driver that executes authorized statements for this registry.
func (r *Registry[S]) SQLAuthority() SQLAuthority { return SQLAuthority{r.statementIssuer} }

// IsZero reports whether this is the unset credential.
func (a SQLAuthority) IsZero() bool { return a.issuer == nil }

// DeniesLegacySQL reports whether this credential requires registered SQL on
// its runner or Direct pool. A zero credential leaves ordinary SQL available.
func (a SQLAuthority) DeniesLegacySQL() bool { return a.issuer != nil && a.issuer.strict }

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
	if s.fixedMethod != 0 {
		return s.fixedMethod == method
	}
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
	method           CallMethod
	shape            *compiledShape
	owner            any
	args             []any
	secret           []bool
	quiet, allSecret bool
}

// Method identifies the intended driver operation. The driver verifies it
// with Open before using the SQL or arguments.
func (c AuthorizedStatement) Method() CallMethod { return c.method }

// AuthorizedSQL is the driver view of a successfully verified call. Its
// fields describe this call only. Editing the view cannot authorize SQL.
type AuthorizedSQL struct {
	Query, Key string
	Args       []any
	Effects    ShapeEffects
	// SecretArgs uses the same bind order as Args.
	SecretArgs       []bool
	table, action    string
	quiet, allSecret bool
}

// ObservationContext attaches the registered key and redaction policy before
// a driver starts observing the call. It preserves the descriptor's logger,
// scope label and treatment of empty results.
func (q AuthorizedSQL) ObservationContext(ctx context.Context) context.Context {
	meta, _ := StatementMetaFrom(ctx)
	meta.Key, meta.Table, meta.Action = q.Key, q.table, q.action
	meta.Quiet, meta.AllSecret = q.quiet, q.allSecret || meta.AllSecret
	meta.SecretArgs = q.SecretArgs
	return WithStatementMeta(ctx, meta)
}

// Open validates a compiled call for the receiving session. Drivers pass their
// own session pointer. The returned parameters are in PostgreSQL bind order.
func (a SQLAuthority) Open(owner any, method CallMethod, call AuthorizedStatement) (AuthorizedSQL, error) {
	if err := a.checkCall(owner, method, call); err != nil {
		return AuthorizedSQL{}, err
	}
	s := call.shape
	return AuthorizedSQL{
		Query: s.sql, Key: s.observationKey, Args: slices.Clone(call.args), Effects: s.shape.Effects,
		SecretArgs: slices.Clone(call.secret), table: s.tableName, action: s.action, quiet: call.quiet, allSecret: call.allSecret,
	}, nil
}

func (a SQLAuthority) checkCall(owner any, method CallMethod, call AuthorizedStatement) error {
	if a.IsZero() || method != call.method || call.shape == nil || call.shape.authority != a.issuer || !eligibleOwner(owner) || call.owner != owner {
		return ErrStatementAuthority
	}
	return nil
}

// AuthorizedSession is the driver channel used for registered queries. Every
// entry verifies its AuthorizedStatement against the receiving session and
// applies the usual method, transaction and database guards.
type AuthorizedSession interface {
	ExecAuthorized(context.Context, AuthorizedStatement) (int64, error)
	ScanAuthorized(context.Context, AuthorizedStatement, ...any) error
	EachAuthorized(context.Context, AuthorizedStatement, func(func(...any) error) error) error
}

// bindCall binds a typed shape to a driver call. Validate the registry's
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
		kind := shape.layout.scopeType
		if r.scopeID != nil {
			scope = kind.snapshot(r.scopeID(s.Scope()))
		} else {
			b := kind.bindScope(s.Scope())
			if b.err != nil {
				return AuthorizedStatement{}, fmt.Errorf("tabula: encode scope: %w", b.err)
			}
			scope = b.value
		}
	}
	args, err := shape.layout.bind(scope, values)
	if err != nil {
		return AuthorizedStatement{}, err
	}
	if r.arrayArgFn != nil {
		if shape.layout.scoped && shape.layout.scopeType.element != nil {
			args[0] = r.arrayArgFn(args[0])
		}
		for _, slot := range shape.layout.order {
			if slot.kind.element != nil {
				i := shape.layout.positions[slot]
				args[i] = r.arrayArgFn(args[i])
			}
		}
	}
	secret := make([]bool, len(shape.shape.Inputs))
	for i, input := range shape.shape.Inputs {
		secret[i] = input.Secret
	}
	return AuthorizedStatement{method: method, shape: shape, owner: s, args: args, secret: secret}, nil
}
