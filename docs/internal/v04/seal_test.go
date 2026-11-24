package v04

import (
	"context"
	"database/sql"
	"errors"
	"iter"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

// These types exist only in the design tests and prototype the decided A4
// proof issuance contract (SessionKey.Sign and SessionProof).
type sessionToken byte
type SessionKey struct{ token *sessionToken }
type SessionProof struct {
	token *sessionToken
	owner any
}

func NewSessionKey() SessionKey { return SessionKey{new(sessionToken)} }

func eligibleOwner(owner any) bool {
	v := reflect.ValueOf(owner)
	return v.Kind() == reflect.Pointer && !v.IsNil() && v.Type().Elem().Size() > 0
}

func (k SessionKey) Sign(owner any) SessionProof {
	if k.token == nil || !eligibleOwner(owner) {
		return SessionProof{}
	}
	return SessionProof{k.token, owner}
}

var ErrForeignSession = errors.New("foreign session")

type registry[S any] struct {
	required bool
	token    atomic.Pointer[sessionToken]
}

func newRegistry[S any](required bool, key SessionKey) *registry[S] {
	r := &registry[S]{required: required}
	r.token.Store(key.token)
	return r
}

func (r *registry[S]) Seal(key SessionKey) error {
	if !r.required || key.token == nil || !r.token.CompareAndSwap(nil, key.token) {
		return errors.New("registry cannot accept this key")
	}
	return nil
}

func (r *registry[S]) check(s tabula.Session[S]) error {
	token := r.token.Load()
	if token == nil && !r.required {
		return nil
	}
	if token == nil || !eligibleOwner(s) {
		return ErrForeignSession
	}
	holder, ok := s.(interface{ SessionProof() SessionProof })
	if !ok {
		return ErrForeignSession
	}
	proof := holder.SessionProof()
	if proof.token != token || proof.owner != s {
		return ErrForeignSession
	}
	return nil
}

type sealRow struct {
	ID    int   `db:"id,key"`
	Scope int64 `db:"scope_id,scope"`
}

type sealTable struct {
	*tabula.Table[sealRow, int, int64]
	reg *registry[int64]
}

type binding struct {
	rows *tabula.Rows[sealRow, int, int64]
	err  error
}

func (table sealTable) Bind(s tabula.Session[int64]) binding {
	if err := table.reg.check(s); err != nil {
		return binding{err: err}
	}
	return binding{rows: table.Table.Bind(s)}
}

func (b binding) Get(ctx context.Context, id int) (*sealRow, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.rows.Get(ctx, id)
}

func (b binding) Iter(ctx context.Context) iter.Seq2[sealRow, error] {
	if b.err == nil {
		return b.rows.Iter(ctx, nil, tabula.Listing{})
	}
	return failedIter[sealRow](b.err)
}

func failedIter[T any](err error) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		yield(zero, err)
	}
}

// The fake only implements the operations used by the successful lookup.
type sealedReader struct {
	tabula.Session[int64]
	proof SessionProof
	calls int
}

func (s *sealedReader) SessionProof() SessionProof { return s.proof }
func (s *sealedReader) Scope() int64 {
	s.calls++
	return 7
}
func (s *sealedReader) Unscoped() bool {
	s.calls++
	return false
}
func (s *sealedReader) ReadOnly() bool {
	s.calls++
	return true
}
func (s *sealedReader) Scan(context.Context, string, []any, ...any) error {
	s.calls++
	return sql.ErrNoRows
}

func TestSealedBind(t *testing.T) {
	open := newRegistry[int64](false, SessionKey{})
	pending := newRegistry[int64](true, SessionKey{})
	descriptors := tabula.NewRegistry(tabula.RegistryConfig[int64]{})
	table := sealTable{tabula.Define(descriptors, tabula.TableSpec[sealRow, int]{Name: "notes"}), pending}
	key := NewSessionKey()
	reader := &sealedReader{}
	reader.proof = key.Sign(reader)
	early := table.Bind(reader)
	require.NoError(t, open.check(reader))
	require.Error(t, open.Seal(key))
	require.Error(t, pending.Seal(SessionKey{}))
	require.NoError(t, pending.Seal(key))
	require.Error(t, pending.Seal(key))
	_, err := early.Get(t.Context(), 1)
	require.ErrorIs(t, err, ErrForeignSession)
	var emitted int
	for row, err := range early.Iter(t.Context()) {
		require.Zero(t, row)
		require.ErrorIs(t, err, ErrForeignSession)
		emitted++
	}
	require.Equal(t, 1, emitted)

	// The decorator promotes the genuine proof accessor. Its pointer is distinct.
	decorated := &struct{ *sealedReader }{reader}
	copied := *reader
	unsigned := &struct{ tabula.Session[int64] }{reader}
	value := struct {
		*sealedReader
		bytes []byte
	}{reader, nil}
	require.Zero(t, key.Sign(new(struct{})))
	require.Zero(t, (SessionKey{}).Sign(reader))
	for _, s := range []tabula.Session[int64]{nil, (*sealedReader)(nil), &sealedReader{}, unsigned, &copied, decorated, value} {
		_, err = table.Bind(s).Get(t.Context(), 1)
		require.ErrorIs(t, err, ErrForeignSession)
	}
	require.ErrorIs(t, newRegistry[int64](false, NewSessionKey()).check(reader), ErrForeignSession)
	require.Zero(t, reader.calls)
	require.Zero(t, copied.calls)
	_, err = table.Bind(reader).Get(t.Context(), 1)
	require.NoError(t, err)
	require.Positive(t, reader.calls)
	copied.proof = key.Sign(&copied)
	require.NoError(t, pending.check(&copied))
}

func TestRegistrySealOnce(t *testing.T) {
	r := newRegistry[int64](true, SessionKey{})
	var wg sync.WaitGroup
	var successes atomic.Int32
	for range 8 {
		wg.Go(func() {
			if r.Seal(NewSessionKey()) == nil {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	require.EqualValues(t, 1, successes.Load())
}
