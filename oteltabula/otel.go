// Package oteltabula bridges tabula.Tracer and tabula.Metrics to the
// OpenTelemetry Go API (go.opentelemetry.io/otel). It lives in its own Go
// module so core tabula packages keep their zero-third-party-dependency
// boundary.
package oteltabula

import (
	"context"
	"slices"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/0xJean/tabula"
)

// InstrumentationName is the default OpenTelemetry instrumentation scope name.
const InstrumentationName = "github.com/0xJean/tabula/oteltabula"

// Option customizes a Tracer or Metrics adapter created by this package.
type Option func(*config)

type config struct {
	name    string
	version string
	attrs   []attribute.KeyValue
}

func resolveConfig(opts []Option) config {
	cfg := config{name: InstrumentationName}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// WithInstrumentationName overrides the OpenTelemetry scope name.
func WithInstrumentationName(name string) Option {
	return func(c *config) {
		if name != "" {
			c.name = name
		}
	}
}

// WithInstrumentationVersion attaches an instrumentation scope version.
func WithInstrumentationVersion(version string) Option {
	return func(c *config) { c.version = version }
}

// WithAttributes appends static attributes (for example db.namespace or
// server.address) to every emitted span and metric observation.
func WithAttributes(attrs ...attribute.KeyValue) Option {
	return func(c *config) {
		c.attrs = append(c.attrs, attrs...)
	}
}

func spanAttrs(info tabula.TraceInfo, extra []attribute.KeyValue) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 6+len(extra))
	attrs = append(attrs, attribute.String("db.system", "postgresql"))
	if info.Key != "" {
		attrs = append(attrs, attribute.String("tabula.key", info.Key))
	}
	if info.Table != "" {
		attrs = append(attrs, attribute.String("tabula.table", info.Table))
	}
	if info.Action != "" {
		attrs = append(attrs, attribute.String("tabula.action", info.Action))
	}
	if info.Role != "" {
		attrs = append(attrs, attribute.String("tabula.role", info.Role))
	}
	if info.Scope != "" {
		attrs = append(attrs, attribute.String("tabula.scope", info.Scope))
	}
	attrs = append(attrs, extra...)
	return attrs
}

// metricAttrs keeps metric cardinality bounded by omitting per-tenant scope
// labels and statement/inline-fingerprint keys (those remain on trace spans).
func metricAttrs(info tabula.TraceInfo, errClass tabula.ErrorClass, extra []attribute.KeyValue) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 5+len(extra))
	attrs = append(attrs, attribute.String("db.system", "postgresql"))
	if info.Table != "" {
		attrs = append(attrs, attribute.String("tabula.table", info.Table))
	}
	if info.Action != "" {
		attrs = append(attrs, attribute.String("tabula.action", info.Action))
	}
	if info.Role != "" {
		attrs = append(attrs, attribute.String("tabula.role", info.Role))
	}
	if errClass != "" {
		attrs = append(attrs, attribute.String("tabula.error_class", string(errClass)))
	}
	attrs = append(attrs, extra...)
	return attrs
}

type otelTracer struct {
	tracer trace.Tracer
	extra  []attribute.KeyValue
}

// NewTracer returns a tabula.Tracer backed by tp (defaulting to
// otel.GetTracerProvider when tp is nil). Attach it to RunnerOptions.Tracer or
// RegistryConfig.Tracer. Failures record the error class and SQLSTATE.
// With RawErrorDiagnostics enabled, exception messages and status descriptions
// also contain the original error text, which may include bound values.
func NewTracer(tp trace.TracerProvider, opts ...Option) tabula.Tracer {
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	cfg := resolveConfig(opts)
	var tOpts []trace.TracerOption
	if cfg.version != "" {
		tOpts = append(tOpts, trace.WithInstrumentationVersion(cfg.version))
	}
	return &otelTracer{
		tracer: tp.Tracer(cfg.name, tOpts...),
		extra:  slices.Clone(cfg.attrs),
	}
}

func (t *otelTracer) Start(ctx context.Context, info tabula.TraceInfo) (context.Context, func(rows int64, err error)) {
	spanName := info.Key
	if spanName == "" {
		spanName = "tabula.statement"
	}
	ctx, span := t.tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(spanAttrs(info, t.extra)...),
	)
	return ctx, func(rows int64, err error) {
		if rows >= 0 {
			span.SetAttributes(attribute.Int64("tabula.rows", rows))
		}
		if err != nil {
			class, _ := tabula.ClassifyError(ctx, err)
			attrs := []attribute.KeyValue{
				attribute.String("tabula.error_class", string(class)),
			}
			if state := tabula.SQLState(err); state != "" {
				attrs = append(attrs, attribute.String("db.response.status_code", state))
			}
			span.SetAttributes(attrs...)
			description := string(class)
			if info.RawErrorDiagnostics {
				span.RecordError(err)
				description = err.Error()
			} else {
				span.AddEvent("exception", trace.WithAttributes(attribute.String("exception.type", string(class))))
			}
			span.SetStatus(codes.Error, description)
		}
		span.End()
	}
}

type otelMetrics struct {
	duration metric.Float64Histogram
	count    metric.Int64Counter
	extra    []attribute.KeyValue
}

// NewMetrics returns a tabula.Metrics backed by mp (defaulting to
// otel.GetMeterProvider when mp is nil). It records tabula.statement.duration
// (seconds) and tabula.statement.count ({statement}) with bounded-cardinality
// attributes (db.system, tabula.table, tabula.action, tabula.role,
// tabula.error_class, plus any static WithAttributes). Per-tenant scope and
// inline statement keys are emitted on trace spans only.
func NewMetrics(mp metric.MeterProvider, opts ...Option) (tabula.Metrics, error) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	cfg := resolveConfig(opts)
	var mOpts []metric.MeterOption
	if cfg.version != "" {
		mOpts = append(mOpts, metric.WithInstrumentationVersion(cfg.version))
	}
	meter := mp.Meter(cfg.name, mOpts...)
	duration, err := meter.Float64Histogram(
		"tabula.statement.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of Tabula PostgreSQL statements in seconds."),
	)
	if err != nil {
		return nil, err
	}
	count, err := meter.Int64Counter(
		"tabula.statement.count",
		metric.WithUnit("{statement}"),
		metric.WithDescription("Total number of Tabula PostgreSQL statements executed."),
	)
	if err != nil {
		return nil, err
	}
	return &otelMetrics{
		duration: duration,
		count:    count,
		extra:    slices.Clone(cfg.attrs),
	}, nil
}

func (m *otelMetrics) Observe(ctx context.Context, info tabula.TraceInfo, duration time.Duration, _ int64, errClass tabula.ErrorClass) {
	opt := metric.WithAttributes(metricAttrs(info, errClass, m.extra)...)
	m.duration.Record(ctx, duration.Seconds(), opt)
	m.count.Add(ctx, 1, opt)
}
