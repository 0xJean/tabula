package tabula

import (
	"context"
	"log/slog"
	"time"
)

// TraceInfo identifies one statement execution for Tracer and Metrics hooks.
// Statement keys double as span names and metric labels.
type TraceInfo struct {
	Key    string
	Table  string
	Action string
	Scope  string
}

// Tracer starts and ends a span around each statement execution. Passed
// explicitly via RegistryConfig.Tracer; nil disables tracing.
type Tracer interface {
	Start(ctx context.Context, info TraceInfo) (context.Context, func(rows int64, err error))
}

// Metrics observes latency, row count, and error class for each statement
// execution. Passed explicitly via RegistryConfig.Metrics; nil disables metrics.
type Metrics interface {
	Observe(ctx context.Context, info TraceInfo, duration time.Duration, rows int64, errClass ErrorClass)
}

type opSpan[S any] struct {
	reg      *Registry[S]
	logger   *slog.Logger
	scope    S
	info     TraceInfo
	start    time.Time
	endTrace func(rows int64, err error)
}

func (r *Registry[S]) startOp(
	ctx context.Context, override *slog.Logger, scope S,
	table, action, key string,
) (context.Context, opSpan[S]) {
	logger := r.effectiveLogger(override)
	logActive := logger != nil && r.log.level.Level() != LogOff
	if !logActive && r.tracer == nil && r.metrics == nil {
		return ctx, opSpan[S]{}
	}
	var scopeStr string
	if r.scopeLog != nil {
		scopeStr = r.scopeLog(scope)
	}
	info := TraceInfo{
		Key:    key,
		Table:  table,
		Action: action,
		Scope:  scopeStr,
	}
	var start time.Time
	if logActive || r.metrics != nil {
		start = r.log.now()
	}
	var endTrace func(int64, error)
	if r.tracer != nil {
		ctx, endTrace = r.tracer.Start(ctx, info)
	}
	return ctx, opSpan[S]{
		reg:      r,
		logger:   override,
		scope:    scope,
		info:     info,
		start:    start,
		endTrace: endTrace,
	}
}

func (s opSpan[S]) finish(
	ctx context.Context, query string, rows int64, err error,
	args []any, secretArg []bool, allSecret, quiet bool,
) {
	if s.reg == nil {
		return
	}
	var dur time.Duration
	if !s.start.IsZero() {
		dur = s.reg.log.now().Sub(s.start)
	}
	if s.endTrace != nil {
		s.endTrace(rows, err)
	}
	if s.reg.metrics != nil {
		var class ErrorClass
		if err != nil {
			class, _ = ClassifyError(ctx, err)
		}
		s.reg.metrics.Observe(ctx, s.info, dur, rows, class)
	}
	s.reg.logEmit(ctx, s.logger, s.info.Scope, logEvent{
		key:       s.info.Key,
		table:     s.info.Table,
		action:    s.info.Action,
		sql:       query,
		duration:  dur,
		rows:      rows,
		err:       err,
		args:      args,
		secretArg: secretArg,
		allSecret: allSecret,
		quiet:     quiet,
	})
}
