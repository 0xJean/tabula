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
	Role   string
}

// Tracer starts and ends a span around each statement execution. Passed
// explicitly via RegistryConfig.Tracer or RunnerOptions.Tracer; nil disables tracing.
type Tracer interface {
	Start(ctx context.Context, info TraceInfo) (context.Context, func(rows int64, err error))
}

// Metrics observes latency, row count, and error class for each statement
// execution. Passed explicitly via RegistryConfig.Metrics or RunnerOptions.Metrics; nil disables metrics.
type Metrics interface {
	Observe(ctx context.Context, info TraceInfo, duration time.Duration, rows int64, errClass ErrorClass)
}

type opSpan[S any] struct {
	reg         *Registry[S]
	logger      *slog.Logger
	scope       S
	info        TraceInfo
	start       time.Time
	endTrace    func(rows int64, err error)
	emitMetrics bool
	emitLog     bool
}

func (r *Registry[S]) startOp(
	ctx context.Context, override *slog.Logger, scope S,
	table, action string, meta stmtMeta, secretArgs []bool, sessionObserved ObserveMask,
) (context.Context, opSpan[S]) {
	var scopeStr string
	if r.scopeLog != nil {
		scopeStr = r.scopeLog(scope)
	}
	if sessionObserved != 0 {
		ctx = WithStatementMeta(ctx, StatementMeta{
			Key:        meta.key,
			Table:      table,
			Action:     action,
			Scope:      scopeStr,
			SecretArgs: secretArgs,
			AllSecret:  meta.secret,
			Quiet:      meta.quiet,
			Logger:     override,
		})
	}
	logger := r.effectiveLogger(override)
	logActive := sessionObserved&ObserveLog == 0 && logger != nil && r.log.level.Level() != LogOff
	traceActive := sessionObserved&ObserveTrace == 0 && r.tracer != nil
	metricsActive := sessionObserved&ObserveMetrics == 0 && r.metrics != nil
	if !logActive && !traceActive && !metricsActive {
		return ctx, opSpan[S]{}
	}
	info := TraceInfo{
		Key:    meta.key,
		Table:  table,
		Action: action,
		Scope:  scopeStr,
	}
	var start time.Time
	if logActive || metricsActive {
		start = r.log.now()
	}
	var endTrace func(int64, error)
	if traceActive {
		ctx, endTrace = r.tracer.Start(ctx, info)
	}
	return ctx, opSpan[S]{
		reg:         r,
		logger:      override,
		scope:       scope,
		info:        info,
		start:       start,
		endTrace:    endTrace,
		emitMetrics: metricsActive,
		emitLog:     logActive,
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
	if s.emitMetrics {
		var class ErrorClass
		if err != nil {
			class, _ = ClassifyError(ctx, err)
		}
		s.reg.metrics.Observe(ctx, s.info, dur, rows, class)
	}
	if s.emitLog {
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
}
