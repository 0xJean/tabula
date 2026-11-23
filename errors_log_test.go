package tabula

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type pgStateErr struct {
	code string
	msg  string
}

func (e pgStateErr) Error() string    { return e.msg }
func (e pgStateErr) SQLState() string { return e.code }

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		class ErrorClass
		want  error
	}{
		{"unique", pgStateErr{"23505", "dup"}, ClassUnique, ErrUnique},
		{"foreign_key", pgStateErr{"23503", "fk"}, ClassForeignKey, ErrForeignKey},
		{"check", pgStateErr{"23514", "chk"}, ClassCheck, ErrCheck},
		{"not_null", pgStateErr{"23502", "nn"}, ClassNotNull, ErrNotNull},
		{"serialization", pgStateErr{"40001", "ser"}, ClassSerialization, ErrSerialization},
		{"deadlock", pgStateErr{"40P01", "dl"}, ClassDeadlock, ErrDeadlock},
		{"rls", pgStateErr{"42501", "new row violates row-level security policy for table items"}, ClassRLSDenied, ErrRLSDenied},
		{"privilege", pgStateErr{"42501", "permission denied for table items"}, ClassInsufficientPrivilege, ErrInsufficientPrivilege},
		{"query_canceled", pgStateErr{"57014", "cancel"}, ClassCanceled, ErrCanceled},
		{"ctx_canceled", context.Canceled, ClassCanceled, ErrCanceled},
		{"other", errors.New("boom"), ClassOther, nil},
	}
	reg := NewRegistry(RegistryConfig[struct{}]{})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotClass, gotSentinel := ClassifyError(t.Context(), tc.err)
			require.Equal(t, tc.class, gotClass)
			require.Equal(t, tc.want, gotSentinel)

			wrapped := reg.classifyErr(t.Context(), tc.err)
			if tc.want != nil {
				require.ErrorIs(t, wrapped, tc.want)
			}
			require.ErrorIs(t, wrapped, tc.err)
		})
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	gotClass, gotSentinel := ClassifyError(ctx, errors.New("driver closed"))
	require.Equal(t, ClassCanceled, gotClass)
	require.Equal(t, ErrCanceled, gotSentinel)
}

type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

type captureHandler struct {
	mu      sync.Mutex
	records []capturedRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, capturedRecord{level: r.Level, msg: r.Message, attrs: attrs})
	h.mu.Unlock()
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }
func (h *captureHandler) reset() {
	h.mu.Lock()
	h.records = nil
	h.mu.Unlock()
}
func (h *captureHandler) snapshot() []capturedRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]capturedRecord, len(h.records))
	copy(out, h.records)
	return out
}

type secretRow struct {
	ID           int64        `db:"id,key"`
	Tenant       customTenant `db:"tenant_id,scope"`
	Username     string       `db:"username"`
	PasswordHash string       `db:"password_hash,secret"`
}

type errSession struct {
	fakeSession
	execErr error
	scanErr error
}

func (e *errSession) Exec(ctx context.Context, q string, args ...any) (int64, error) {
	if e.execErr != nil {
		return 0, e.execErr
	}
	return e.fakeSession.Exec(ctx, q, args...)
}
func (e *errSession) Scan(ctx context.Context, q string, args []any, dest ...any) error {
	if e.scanErr != nil {
		return e.scanErr
	}
	return e.fakeSession.Scan(ctx, q, args, dest...)
}

func TestLoggingSpec(t *testing.T) {
	// Deterministic clock: each call advances by step.
	var (
		base = time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
		step = 10 * time.Millisecond
		tick int
	)
	nowFn := func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * step)
	}

	handler := &captureHandler{}
	logger := slog.New(handler)
	levelVar := NewLogLevelVar(LogOff)

	reg := NewRegistry(RegistryConfig[customTenant]{
		ScopeID:  func(s customTenant) any { return string(s) },
		ScopeLog: func(s customTenant) string { return string(s) },
		Errors: ErrorPolicy{
			Sanitize: func(err error) string { return "sanitized:" + SQLState(err) },
		},
		Log: LogConfig{
			Logger:        logger,
			LevelVar:      levelVar,
			SlowThreshold: 200 * time.Millisecond,
			Values:        ValuesNever,
			SQL:           true,
			Now:           nowFn,
		},
	})
	tbl := Define(reg, TableSpec[secretRow, int64]{Name: "public.accounts"})
	quietStmt := tbl.Statement("heartbeat", "SELECT 1", Quiet())
	secretStmt := tbl.Statement("verify_secret", "SELECT "+tbl.Cols()+" FROM public.accounts WHERE password_hash = $1", Secret())

	sess := &errSession{fakeSession: fakeSession{tenant: "t-9", execRows: 1}}
	rows := tbl.Bind(sess)

	// 1. LogOff: nothing reaches the handler, even on error.
	sess.execErr = pgStateErr{"23505", "dup key secret_val"}
	require.ErrorIs(t, rows.Insert(t.Context(), &secretRow{ID: 1, Tenant: "t-9", Username: "alice", PasswordHash: "SUPER_SECRET"}), ErrUnique)
	require.Empty(t, handler.snapshot())

	// 2. LogErrors: logs failures with exact fixed keys, sanitized error, never argument values.
	levelVar.Set(LogErrors)
	require.ErrorIs(t, rows.Insert(t.Context(), &secretRow{ID: 1, Tenant: "t-9", Username: "alice", PasswordHash: "SUPER_SECRET"}), ErrUnique)
	recs := handler.snapshot()
	require.Len(t, recs, 1)
	rec := recs[0]
	require.Equal(t, slog.LevelError, rec.level)
	require.Equal(t, "tabula.statement_failed", rec.msg)
	require.Equal(t, "public.accounts.insert", rec.attrs["tabula.key"])
	require.Equal(t, "public.accounts", rec.attrs["tabula.table"])
	require.Equal(t, "insert", rec.attrs["tabula.action"])
	require.Equal(t, float64(10), rec.attrs["tabula.duration_ms"])
	require.Equal(t, int64(0), rec.attrs["tabula.rows"])
	require.Equal(t, "unique", rec.attrs["tabula.error_class"])
	require.Equal(t, "23505", rec.attrs["tabula.sqlstate"])
	require.Equal(t, "sanitized:23505", rec.attrs["tabula.error"])
	require.Equal(t, "t-9", rec.attrs["tabula.scope"])
	require.NotContains(t, rec.attrs, "tabula.args")
	require.NotContains(t, rec.attrs, "tabula.sql")
	for _, v := range rec.attrs {
		require.NotContains(t, strings.ToUpper(formatArgValue(v)), "SUPER_SECRET")
	}

	// Fast success is NOT logged under LogErrors or LogSlow.
	handler.reset()
	sess.execErr = nil
	levelVar.Set(LogSlow)
	require.NoError(t, rows.Insert(t.Context(), &secretRow{ID: 2, Tenant: "t-9", Username: "bob", PasswordHash: "TOP_SECRET"}))
	require.Empty(t, handler.snapshot())

	// 3. LogSlow: slow statement emits warn with tabula.slow_statement.
	step = 250 * time.Millisecond
	require.NoError(t, rows.Insert(t.Context(), &secretRow{ID: 3, Tenant: "t-9", Username: "carol", PasswordHash: "TOP_SECRET"}))
	recs = handler.snapshot()
	require.Len(t, recs, 1)
	require.Equal(t, slog.LevelWarn, recs[0].level)
	require.Equal(t, "tabula.slow_statement", recs[0].msg)
	require.Equal(t, float64(250), recs[0].attrs["tabula.duration_ms"])

	// Quiet() suppresses LogSlow and LogAll on success, but still logs failures.
	handler.reset()
	var one int
	require.NoError(t, rows.Scan(t.Context(), quietStmt, nil, &one))
	require.Empty(t, handler.snapshot())
	levelVar.Set(LogAll)
	require.NoError(t, rows.Scan(t.Context(), quietStmt, nil, &one))
	require.Empty(t, handler.snapshot())
	sess.scanErr = errors.New("db down")
	require.Error(t, rows.Scan(t.Context(), quietStmt, nil, &one))
	require.Len(t, handler.snapshot(), 1)
	require.Equal(t, "public.accounts.heartbeat", handler.snapshot()[0].attrs["tabula.key"])
	sess.scanErr = nil

	// 4. ValuesAllowed: non-secret columns logged, secret column & Secret() stmt always "[redacted]".
	handler.reset()
	step = 5 * time.Millisecond
	regAllowed := NewRegistry(RegistryConfig[customTenant]{
		ScopeID: func(s customTenant) any { return string(s) },
		Log: LogConfig{
			Logger: logger,
			Level:  LogAll,
			Values: ValuesAllowed,
			SQL:    true,
			Now:    nowFn,
		},
	})
	tbl2 := Define(regAllowed, TableSpec[secretRow, int64]{Name: "public.accounts"})
	secretStmt2 := tbl2.Statement("verify_secret", secretStmt, Secret())
	rows2 := tbl2.Bind(sess)

	require.NoError(t, rows2.Insert(t.Context(), &secretRow{ID: 4, Tenant: "t-9", Username: "dave", PasswordHash: "NEVER_LEAK_ME"}))
	recs = handler.snapshot()
	require.Len(t, recs, 1)
	require.Equal(t, "tabula.statement", recs[0].msg)
	require.Contains(t, recs[0].attrs, "tabula.sql")
	argsVal := recs[0].attrs["tabula.args"].([]string)
	require.Equal(t, []string{"4", "t-9", "dave", "[redacted]"}, argsVal)

	handler.reset()
	_, err := rows2.Select(t.Context(), secretStmt2, "MY_API_TOKEN")
	require.NoError(t, err)
	recs = handler.snapshot()
	require.Len(t, recs, 1)
	require.Equal(t, "public.accounts.verify_secret", recs[0].attrs["tabula.key"])
	require.Equal(t, []string{"[redacted]"}, recs[0].attrs["tabula.args"].([]string))

	// 5. ValuesRedacted: logs only Go type and length.
	handler.reset()
	regRedacted := NewRegistry(RegistryConfig[customTenant]{
		ScopeID: func(s customTenant) any { return string(s) },
		Log: LogConfig{
			Logger: logger,
			Level:  LogAll,
			Values: ValuesRedacted,
			Now:    nowFn,
		},
	})
	tbl3 := Define(regRedacted, TableSpec[secretRow, int64]{Name: "public.accounts"})
	require.NoError(t, tbl3.Bind(sess).Insert(t.Context(), &secretRow{ID: 5, Tenant: "t-9", Username: "eve", PasswordHash: "HIDDEN"}))
	recs = handler.snapshot()
	require.Len(t, recs, 1)
	require.Equal(t, []string{"int64", "tabula.customTenant(3)", "string(3)", "[redacted]"}, recs[0].attrs["tabula.args"].([]string))

	// 6. Sampling on LogAll (0.5 logs half) and per-binding WithLogger override.
	overrideHandler := &captureHandler{}
	regSample := NewRegistry(RegistryConfig[customTenant]{
		ScopeID: func(s customTenant) any { return string(s) },
		Log: LogConfig{
			Logger: nil, // disabled on registry, enabled via WithLogger
			Level:  LogAll,
			Sample: 0.5,
			Now:    nowFn,
		},
	})
	tbl4 := Define(regSample, TableSpec[secretRow, int64]{Name: "public.accounts"})
	bound := tbl4.Bind(sess).WithLogger(slog.New(overrideHandler))
	for i := range 10 {
		require.NoError(t, bound.Insert(t.Context(), &secretRow{ID: int64(i + 1), Tenant: "t-9", Username: "u", PasswordHash: "p"}))
	}
	require.Len(t, overrideHandler.snapshot(), 5)
}
