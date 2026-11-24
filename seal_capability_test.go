package tabula

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

type provenSession struct {
	fakeSession
	proof SessionProof
	calls int
}

func (s *provenSession) SessionProof() SessionProof { return s.proof }
func (s *provenSession) Scope() customTenant {
	s.calls++
	return s.fakeSession.Scope()
}
func (s *provenSession) Unscoped() bool {
	s.calls++
	return s.fakeSession.Unscoped()
}
func (s *provenSession) Scan(ctx context.Context, q string, args []any, dest ...any) error {
	s.calls++
	return s.fakeSession.Scan(ctx, q, args, dest...)
}
func (s *provenSession) Exec(ctx context.Context, q string, args ...any) (int64, error) {
	s.calls++
	return s.fakeSession.Exec(ctx, q, args...)
}
func (s *provenSession) Each(ctx context.Context, q string, args []any, dec func(func(...any) error) error) error {
	s.calls++
	return s.fakeSession.Each(ctx, q, args, dec)
}

func TestSealedSessionsAndRegistrySeal(t *testing.T) {
	openReg := NewRegistry(RegistryConfig[customTenant]{})
	pendingReg := NewRegistry(RegistryConfig[customTenant]{RequireSealed: true})
	tbl := Define(pendingReg, TableSpec[customRow, int64]{Name: "public.sealed_items", Order: "id"})
	view := DefineView(pendingReg, ViewSpec[cqRow]{Name: "sealed_view"})

	key := NewSessionKey()
	require.False(t, key.IsZero())
	require.True(t, (SessionKey{}).IsZero())
	require.Zero(t, (SessionKey{}).Sign(&provenSession{}))
	require.Zero(t, key.Sign(nil))
	require.Zero(t, key.Sign(new(struct{})))

	sess := &provenSession{fakeSession: fakeSession{tenant: "t-1", scanFn: func(...any) error { return sql.ErrNoRows }}}
	sess.proof = key.Sign(sess)

	// Open registry accepts unsealed sessions and rejects Seal.
	require.NoError(t, openReg.checkSession(sess))
	require.Error(t, openReg.Seal(key))

	// Pending registry rejects Bind before Seal.
	early := tbl.Bind(sess)
	earlyView := view.Bind(sess)
	require.Error(t, pendingReg.Seal(SessionKey{}))
	require.NoError(t, pendingReg.Seal(key))
	require.Error(t, pendingReg.Seal(key)) // second call fails even with same key

	// Early binding stays permanently rejected even after Seal.
	_, err := early.Get(t.Context(), 1)
	require.ErrorIs(t, err, ErrForeignSession)
	require.ErrorIs(t, early.CheckCopy(), ErrForeignSession)
	require.ErrorIs(t, early.CheckWrite(&customRow{Tenant: "t-1"}), ErrForeignSession)
	require.Equal(t, " AND (FALSE)", early.Scope().Where())
	_, err = early.WithLogger(slog.Default()).Get(t.Context(), 1)
	require.ErrorIs(t, err, ErrForeignSession)
	_, err = earlyView.One(t.Context(), "SELECT 1")
	require.ErrorIs(t, err, ErrForeignSession)
	var emitted int
	for row, iterErr := range early.Iter(t.Context(), nil, Listing{}) {
		require.Zero(t, row)
		require.ErrorIs(t, iterErr, ErrForeignSession)
		emitted++
	}
	require.Equal(t, 1, emitted)
	require.Zero(t, sess.calls)

	// Forged, copied, wrapped, nil, typed-nil and value-receiver sessions fail.
	decorated := &struct{ *provenSession }{sess}
	copied := *sess
	unsigned := &struct{ Session[customTenant] }{sess}
	valueSess := struct {
		*provenSession
		extra []byte
	}{sess, nil}
	for _, bad := range []Session[customTenant]{nil, (*provenSession)(nil), &provenSession{}, unsigned, &copied, decorated, valueSess} {
		_, err = tbl.Bind(bad).Get(t.Context(), 1)
		require.ErrorIs(t, err, ErrForeignSession)
	}
	require.Zero(t, sess.calls)
	require.Zero(t, copied.calls)

	// Genuine signed pointer succeeds after Seal.
	got, err := tbl.Bind(sess).Get(t.Context(), 1)
	require.NoError(t, err)
	require.Nil(t, got)
	require.Positive(t, sess.calls)

	// Concurrent Seal allows exactly one winner.
	raceReg := NewRegistry(RegistryConfig[customTenant]{RequireSealed: true})
	var wg sync.WaitGroup
	var wins atomic.Int32
	for range 8 {
		wg.Go(func() {
			if raceReg.Seal(NewSessionKey()) == nil {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	require.EqualValues(t, 1, wins.Load())
}

func BenchmarkSealedBind(b *testing.B) {
	key := NewSessionKey()
	reg := NewRegistry(RegistryConfig[customTenant]{SessionKey: key})
	tbl := Define(reg, TableSpec[customRow, int64]{Name: "public.bench_items"})
	sess := &provenSession{fakeSession: fakeSession{tenant: "t-1"}}
	sess.proof = key.Sign(sess)
	b.ReportAllocs()
	for b.Loop() {
		_ = tbl.Bind(sess)
	}
}

func TestCapabilityPolicyAndTransactionMarker(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{
		Deny: DenyDelete | DenyUpsert | DenyUnscopedWrites | DenyCopy,
	})
	_, err := TryDefine(reg, TableSpec[customRow, int64]{Name: "public.items", AllowDelete: true})
	require.ErrorIs(t, err, ErrCapabilityDenied)

	tbl := Define(reg, TableSpec[customRow, int64]{Name: "public.items"})
	_, err = tbl.TryDefineUpsert("up", UpsertSpec{Conflict: []string{"id"}})
	require.ErrorIs(t, err, ErrCapabilityDenied)

	unscopedSess := &fakeSession{unscoped: true}
	r := tbl.Bind(unscopedSess)
	require.ErrorIs(t, r.CheckCopy(), ErrCapabilityDenied)
	require.ErrorIs(t, r.CheckWrite(&customRow{}), ErrCapabilityDenied)
	require.ErrorIs(t, r.Insert(t.Context(), &customRow{}), ErrCapabilityDenied)
	require.ErrorIs(t, r.Exec(t.Context(), 1, "UPDATE public.items SET code = 'x'"), ErrCapabilityDenied)

	scopedSess := &fakeSession{tenant: "t-1"}
	rs := tbl.Bind(scopedSess)
	require.ErrorIs(t, rs.CheckCopy(), ErrCapabilityDenied) // DenyCopy even when scoped

	// Cross-module transaction marker preserves other context values.
	type ctxKey struct{}
	base := context.WithValue(t.Context(), ctxKey{}, "req-42")
	require.False(t, InTransaction(base))
	marked := MarkTransaction(base)
	require.True(t, InTransaction(marked))
	require.Equal(t, "req-42", marked.Value(ctxKey{}))
	detached := WithoutTransaction(marked)
	require.False(t, InTransaction(detached))
	require.Equal(t, "req-42", detached.Value(ctxKey{}))
}

func TestObserverMetadataPrecedenceAndRedaction(t *testing.T) {
	var runnerBuf, regBuf bytes.Buffer
	runnerLog := slog.New(slog.NewJSONHandler(&runnerBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	regLog := slog.New(slog.NewJSONHandler(&regBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	runnerTracer := &fakeTracer{}
	regTracer := &fakeTracer{}

	obs := NewObserver(ObserverConfig{
		Role:          "app_writer",
		Tracer:        runnerTracer,
		TagStatements: true,
		Log:           LogConfig{Logger: runnerLog, Level: LogAll, Values: ValuesAllowed, SQL: true},
	})
	require.Equal(t, ObserveTrace|ObserveLog|ObserveTag, obs.Signals())

	reg := NewRegistry(RegistryConfig[customTenant]{
		ScopeLog: func(s customTenant) string { return string(s) },
		Tracer:   regTracer,
		Log:      LogConfig{Logger: regLog, Level: LogAll, Values: ValuesAllowed},
	})
	tbl := Define(reg, TableSpec[customRow, int64]{Name: "public.items"})
	secretStmt := tbl.Statement("secret_read", "SELECT "+tbl.Cols()+" FROM public.items WHERE code = $1", Secret())

	sess := &observedFakeSession{
		fakeSession: fakeSession{tenant: "t-9"},
		obs:         obs,
	}
	_, err := tbl.Bind(sess).Select(t.Context(), secretStmt, "top-secret-token")
	require.NoError(t, err)
	// Runner owns trace + log; registry must not duplicate them.
	require.Len(t, runnerTracer.spans, 1)
	require.Empty(t, regTracer.spans)
	require.Equal(t, "public.items.secret_read", runnerTracer.spans[0].info.Key)
	require.Equal(t, "app_writer", runnerTracer.spans[0].info.Role)
	require.NotContains(t, runnerBuf.String(), "top-secret-token")
	require.Contains(t, runnerBuf.String(), "[redacted]")
	require.Empty(t, regBuf.String())
	require.Contains(t, sess.lastTagged, "/* tabula:public.items.secret_read */")

	// Inline SQL uses normalized fingerprint without argument values.
	require.Equal(t, SQLFingerprint("SELECT id FROM items WHERE code = 'abc' AND n = 1"), SQLFingerprint("SELECT id FROM items WHERE code = 'xyz' AND n = 99"))
	require.Equal(t, "select", SQLAction("WITH cte AS (SELECT 1) SELECT * FROM cte"))
	require.Equal(t, "update", SQLAction("UPDATE items SET code = $1"))
}

type observedFakeSession struct {
	fakeSession
	obs        *Observer
	lastTagged string
}

func (s *observedFakeSession) TabulaObserved() ObserveMask { return s.obs.Signals() }
func (s *observedFakeSession) Each(ctx context.Context, q string, args []any, dec func(func(...any) error) error) error {
	s.lastTagged = s.obs.TagSQL(ctx, q)
	ctx, done := s.obs.Start(ctx, string(s.tenant), q, args)
	err := s.fakeSession.Each(ctx, q, args, dec)
	done(0, err)
	return err
}

func TestSQLFilesAndAggregatesUnit(t *testing.T) {
	fsys := fstest.MapFS{
		"sql/orders.sql":     {Data: []byte("SELECT id FROM orders\n")},
		"sql/sub/report.sql": {Data: []byte("SELECT 1")},
		"sql/readme.txt":     {Data: []byte("ignored")},
	}
	sf := MustSQLFiles(fsys, "sql")
	require.Equal(t, []string{"orders.sql", "sub/report.sql"}, sf.Names())
	require.Equal(t, "SELECT id FROM orders", sf.Get("orders.sql"))
	require.Equal(t, "SELECT 1", sf.Get("sub/report.sql"))
	require.Panics(t, func() { _ = sf.Get("missing.sql") })
	_, err := LoadSQLFiles(fsys, "nonexistent")
	require.Error(t, err)

	// GroupBy validation + statement recording.
	reg := NewRegistry(RegistryConfig[customTenant]{})
	_, err = TryDefine(reg, TableSpec[customRow, int64]{Name: "public.items", GroupBy: []string{"nope"}})
	require.Error(t, err)
	tbl := Define(reg, TableSpec[customRow, int64]{Name: "public.items", GroupBy: []string{"code"}})
	require.Contains(t, tbl.Definition().Statements, "count_by_code")
}
