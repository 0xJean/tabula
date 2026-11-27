package tabula

import (
	"context"
	"slices"

	"github.com/0xJean/tabula/internal/planrt"
)

// SQLAuthority verifies registered statements for a driver on behalf of one
// registry. It cannot sign SQL or change which statements that registry owns.
type SQLAuthority struct{ issuer *planrt.Issuer }

// SQLAuthority returns this registry's verification credential. Pass it to a
// driver that executes authorized statements for this registry.
func (r *Registry[S]) SQLAuthority() SQLAuthority { return SQLAuthority{r.statementIssuer} }

// IsZero reports whether this is the unset credential.
func (a SQLAuthority) IsZero() bool { return a.issuer == nil }

// DeniesLegacySQL reports whether this credential requires registered SQL on
// its runner or Direct pool. A zero credential leaves ordinary SQL available.
func (a SQLAuthority) DeniesLegacySQL() bool { return a.issuer != nil && a.issuer.Strict }

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

// AuthorizedStatement is an opaque call bound to one registered shape, one
// session instance and its ordered parameters. Applications cannot construct it
// from SQL text, a snapshot or a verification credential.
type AuthorizedStatement struct {
	method           CallMethod
	shape            *planrt.CompiledShape
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
		Query: s.SQL, Key: s.ObservationKey, Args: slices.Clone(call.args), Effects: ShapeEffects(s.Effects),
		SecretArgs: slices.Clone(call.secret), table: s.TableName, action: s.Action, quiet: call.quiet, allSecret: call.allSecret,
	}, nil
}

func (a SQLAuthority) checkCall(owner any, method CallMethod, call AuthorizedStatement) error {
	if a.IsZero() || method != call.method || call.shape == nil || call.shape.Authority != a.issuer || !eligibleOwner(owner) || call.owner != owner {
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
