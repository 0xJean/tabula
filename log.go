package tabula

import (
	"context"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"sync/atomic"
	"time"
)

// LogLevel controls which statement executions are emitted to the configured
// slog.Logger.
type LogLevel int32

const (
	// LogOff disables statement logging completely (default).
	LogOff LogLevel = iota
	// LogErrors logs failed statements only.
	LogErrors
	// LogSlow logs failed statements and statements slower than SlowThreshold.
	LogSlow
	// LogAll logs every statement (for debugging; not for production traffic).
	LogAll
)

// LogValues controls whether bound argument values appear in statement logs.
type LogValues int

const (
	// ValuesNever omits all bound argument values from log output (default).
	ValuesNever LogValues = iota
	// ValuesRedacted logs only each argument's Go type and length.
	ValuesRedacted
	// ValuesAllowed logs bound argument values except secret columns and
	// statements marked with Secret(), which are always replaced by "[redacted]".
	ValuesAllowed
)

// LogLevelVar is an atomic runtime switch for LogLevel that can be shared
// across one or more registries or adjusted without restarting.
type LogLevelVar struct {
	v atomic.Int32
}

// NewLogLevelVar creates a LogLevelVar initialized to level.
func NewLogLevelVar(level LogLevel) *LogLevelVar {
	lv := &LogLevelVar{}
	lv.Set(level)
	return lv
}

// Level returns the current LogLevel.
func (l *LogLevelVar) Level() LogLevel { return LogLevel(l.v.Load()) }

// Set atomically updates the active LogLevel.
func (l *LogLevelVar) Set(level LogLevel) { l.v.Store(int32(level)) }

// LogConfig configures opt-in structured logging via log/slog on a Registry.
// When Logger is nil, logging is completely off and costs one nil check.
type LogConfig struct {
	Logger        *slog.Logger
	Level         LogLevel
	LevelVar      *LogLevelVar
	SlowThreshold time.Duration
	Values        LogValues
	// Sample applies to LogAll only (0 < Sample < 1 samples that fraction;
	// 0 or >= 1 logs all statements). Errors and slow statements are never sampled.
	Sample float64
	// SQL includes the statement SQL as "tabula.sql" when Level is LogAll.
	SQL bool
	// Now overrides the clock used to measure statement duration (optional;
	// defaults to time.Now only when Logger is non-nil).
	Now func() time.Time
}

// StatementOption configures per-statement logging behavior at registration time.
type StatementOption func(*stmtMeta)

// Quiet suppresses non-error logging for a registered statement (never logged
// under LogSlow or LogAll; errors are still logged).
func Quiet() StatementOption { return func(m *stmtMeta) { m.quiet = true } }

// Secret forces every bound argument of a registered statement to be redacted
// as "[redacted]" even when LogConfig.Values is ValuesAllowed.
func Secret() StatementOption { return func(m *stmtMeta) { m.secret = true } }

type stmtMeta struct {
	key    string
	quiet  bool
	secret bool
}

type logEvent struct {
	key       string
	table     string
	action    string
	sql       string
	duration  time.Duration
	rows      int64 // < 0 when unknown
	err       error
	args      []any
	secretArg []bool
	allSecret bool
	quiet     bool
}

type logState struct {
	logger        *slog.Logger
	level         *LogLevelVar
	slowThreshold time.Duration
	values        LogValues
	sample        float64
	includeSQL    bool
	now           func() time.Time
	sampleCounter atomic.Uint64
}

func newLogState(cfg LogConfig) *logState {
	lv := cfg.LevelVar
	if lv == nil {
		lv = NewLogLevelVar(cfg.Level)
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &logState{
		logger:        cfg.Logger,
		level:         lv,
		slowThreshold: cfg.SlowThreshold,
		values:        cfg.Values,
		sample:        cfg.Sample,
		includeSQL:    cfg.SQL,
		now:           nowFn,
	}
}

func (r *Registry[S]) effectiveLogger(override *slog.Logger) *slog.Logger {
	if override != nil {
		return override
	}
	if r.log == nil {
		return nil
	}
	return r.log.logger
}

func (r *Registry[S]) logEmit(ctx context.Context, override *slog.Logger, scopeStr string, ev logEvent) {
	logger := r.effectiveLogger(override)
	if logger == nil {
		return
	}
	level := r.log.level.Level()
	if level == LogOff {
		return
	}

	var (
		slogLevel slog.Level
		msg       string
	)
	switch {
	case ev.err != nil:
		if level < LogErrors {
			return
		}
		slogLevel = slog.LevelError
		msg = "tabula.statement_failed"
	case ev.quiet:
		return
	case r.log.slowThreshold > 0 && ev.duration >= r.log.slowThreshold && level >= LogSlow:
		slogLevel = slog.LevelWarn
		msg = "tabula.slow_statement"
	case level == LogAll:
		if !r.log.shouldSample() {
			return
		}
		slogLevel = slog.LevelInfo
		msg = "tabula.statement"
	default:
		return
	}

	attrs := make([]slog.Attr, 0, 10)
	attrs = append(attrs,
		slog.String("tabula.key", ev.key),
		slog.String("tabula.table", ev.table),
		slog.String("tabula.action", ev.action),
		slog.Float64("tabula.duration_ms", float64(ev.duration.Nanoseconds())/1e6),
	)
	if ev.rows >= 0 {
		attrs = append(attrs, slog.Int64("tabula.rows", ev.rows))
	}
	if ev.err != nil {
		class, _ := ClassifyError(ctx, ev.err)
		attrs = append(attrs, slog.String("tabula.error_class", string(class)))
		if state := SQLState(ev.err); state != "" {
			attrs = append(attrs, slog.String("tabula.sqlstate", state))
		}
		errMsg := ev.err.Error()
		if r.errors.Sanitize != nil {
			errMsg = r.errors.Sanitize(ev.err)
		}
		attrs = append(attrs, slog.String("tabula.error", errMsg))
	}
	if scopeStr != "" {
		attrs = append(attrs, slog.String("tabula.scope", scopeStr))
	}
	if level == LogAll && r.log.includeSQL && ev.sql != "" {
		attrs = append(attrs, slog.String("tabula.sql", ev.sql))
	}
	if r.log.values != ValuesNever && len(ev.args) > 0 {
		attrs = append(attrs, slog.Any("tabula.args", formatLogArgs(r.log.values, ev.args, ev.secretArg, ev.allSecret)))
	}
	logger.LogAttrs(ctx, slogLevel, msg, attrs...)
}

func (ls *logState) shouldSample() bool {
	if ls.sample <= 0 || ls.sample >= 1 {
		return true
	}
	// Deterministic fractional sampling using floor(n*p) > floor((n-1)*p):
	// race-free, zero RNG state, and exact rate over any run length.
	n := ls.sampleCounter.Add(1)
	return math.Floor(float64(n)*ls.sample) > math.Floor(float64(n-1)*ls.sample)
}

func formatLogArgs(mode LogValues, args []any, secretIdx []bool, allSecret bool) []string {
	out := make([]string, len(args))
	for i, arg := range args {
		isSecret := allSecret || (i < len(secretIdx) && secretIdx[i])
		if isSecret {
			out[i] = "[redacted]"
			continue
		}
		switch mode {
		case ValuesRedacted:
			out[i] = redactArgTypeLen(arg)
		case ValuesAllowed:
			out[i] = formatArgValue(arg)
		}
	}
	return out
}

func redactArgTypeLen(arg any) string {
	v := unwrapArgValue(arg)
	if v == nil {
		return "nil"
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String, reflect.Slice, reflect.Array, reflect.Map:
		return fmt.Sprintf("%s(%d)", rv.Type().String(), rv.Len())
	default:
		return rv.Type().String()
	}
}

func formatArgValue(arg any) string {
	v := unwrapArgValue(arg)
	if v == nil {
		return "<nil>"
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}

func unwrapArgValue(arg any) any {
	if arg == nil {
		return nil
	}
	if valuer, ok := arg.(driver.Valuer); ok {
		if v, err := valuer.Value(); err == nil {
			return v
		}
	}
	rv := reflect.ValueOf(arg)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		return rv.Elem().Interface()
	}
	return arg
}
