package tabula

import (
	"context"
	"errors"
	"reflect"
)

type sessionToken struct{ _ byte }

// SessionKey is an opaque, unforgeable signing authority created by trusted
// application startup code (NewSessionKey). Registries configured with
// SessionKey or RequireSealed accept only sessions carrying a matching
// SessionProof signed for that exact session pointer instance.
type SessionKey struct {
	token *sessionToken
}

// SessionProof records that a specific concrete session pointer was issued by
// the holder of a SessionKey. Its fields are unexported and bound to both the
// key token and the concrete owner pointer, so copying a proof onto another
// struct or wrapping a session fails verification.
type SessionProof struct {
	token *sessionToken
	owner any
}

// NewSessionKey allocates a fresh, distinct SessionKey.
func NewSessionKey() SessionKey {
	return SessionKey{token: new(sessionToken)}
}

// IsZero reports whether k is the unset zero SessionKey.
func (k SessionKey) IsZero() bool { return k.token == nil }

// Sign issues a SessionProof binding k to owner. owner must be a non-nil
// pointer to a nonzero-size value; otherwise (or when k is zero) Sign returns
// the zero SessionProof.
func (k SessionKey) Sign(owner any) SessionProof {
	if k.token == nil || !eligibleOwner(owner) {
		return SessionProof{}
	}
	return SessionProof{token: k.token, owner: owner}
}

func eligibleOwner(owner any) bool {
	if owner == nil {
		return false
	}
	v := reflect.ValueOf(owner)
	return v.Kind() == reflect.Pointer && !v.IsNil() && v.Type().Elem().Size() > 0
}

// Seal atomically attaches key to a Registry created with RequireSealed: true
// and a zero SessionKey. It fails on an open registry, a zero key, or any
// registry that already has a key (including a repeat call with the same key).
func (r *Registry[S]) Seal(key SessionKey) error {
	if r == nil || !r.requireSealed || key.token == nil || !r.sessionToken.CompareAndSwap(nil, key.token) {
		prefix := "tabula"
		if r != nil && r.prefix != "" {
			prefix = r.prefix
		}
		return errors.New(prefix + ".Seal: registry cannot accept this session key")
	}
	return nil
}

func (r *Registry[S]) checkSession(s Session[S]) error {
	if r == nil {
		return nil
	}
	tok := r.sessionToken.Load()
	if tok == nil && !r.requireSealed {
		return nil
	}
	if tok == nil || !eligibleOwner(s) {
		return ErrForeignSession
	}
	holder, ok := s.(interface{ SessionProof() SessionProof })
	if !ok {
		return ErrForeignSession
	}
	proof := holder.SessionProof()
	if proof.token != tok || proof.owner != any(s) {
		return ErrForeignSession
	}
	return nil
}

type txContextKey struct{}

// A nonzero-sized allocation gives each checked callback a distinct identity.
type txMarker struct{ callback *byte }

// BindCallbackContext marks a new runner callback and returns a check for its
// context and descendants. The check returns ErrCallbackContext for unrelated
// or detached contexts. It retains only the private identity.
func BindCallbackContext(ctx context.Context) (context.Context, func(context.Context) error) {
	if ctx == nil {
		ctx = context.Background()
	}
	marker := txMarker{callback: new(byte)}
	return context.WithValue(ctx, txContextKey{}, marker), func(candidate context.Context) error {
		if candidate == nil || candidate.Value(txContextKey{}) != marker {
			return ErrCallbackContext
		}
		return nil
	}
}

// MarkTransaction marks ctx as running inside an active runner transaction so
// nested runner entries (across both stdsql and pgxdriver) fail closed with
// ErrNested before BEGIN.
func MarkTransaction(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if InTransaction(ctx) {
		return ctx
	}
	return context.WithValue(ctx, txContextKey{}, txMarker{})
}

// InTransaction reports whether ctx carries an active transaction marker from
// MarkTransaction (and has not been detached with WithoutTransaction).
func InTransaction(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, active := ctx.Value(txContextKey{}).(txMarker)
	return active
}

// WithoutTransaction clears the transaction marker and callback ownership while
// preserving deadlines, cancellation, tracing spans and request metadata.
//
// Use this only when intentionally opening an independent transaction inside a
// runner callback (for example, an out-of-band audit record that must commit
// even if the outer transaction rolls back). Warning: opening a detached
// transaction checks out a second pooled connection while the outer transaction
// still holds one, which can deadlock a saturated connection pool.
func WithoutTransaction(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, txContextKey{}, false)
}
