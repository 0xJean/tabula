package oteltabula_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/oteltabula"
	"github.com/stretchr/testify/require"
)

type fakePGError struct{ code string }

func (e fakePGError) Error() string    { return "pg error " + e.code }
func (e fakePGError) SQLState() string { return e.code }

func TestTracerRecordsSuccessAndErrorSpans(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tr := oteltabula.NewTracer(tp,
		oteltabula.WithInstrumentationName("custom.tabula"),
		oteltabula.WithInstrumentationVersion("0.3.1"),
		oteltabula.WithAttributes(attribute.String("db.namespace", "appdb")),
	)

	// 1. Successful statement span.
	info := tabula.TraceInfo{
		Key:    "orders.list",
		Table:  "orders",
		Action: "list",
		Scope:  "t-1",
		Role:   "app_reader",
	}
	ctx, end := tr.Start(t.Context(), info)
	end(4, nil)
	require.NotNil(t, ctx)

	// 2. Failed statement span with SQLSTATE + default empty name fallback.
	_, endErr := tr.Start(t.Context(), tabula.TraceInfo{
		Action: "insert",
		Role:   "app_writer",
	})
	endErr(0, fakePGError{code: "23505"})

	spans := sr.Ended()
	require.Len(t, spans, 2)

	okSpan := spans[0]
	require.Equal(t, "orders.list", okSpan.Name())
	require.Equal(t, trace.SpanKindClient, okSpan.SpanKind())
	require.Equal(t, "custom.tabula", okSpan.InstrumentationScope().Name)
	require.Equal(t, "0.3.1", okSpan.InstrumentationScope().Version)
	require.Equal(t, codes.Unset, okSpan.Status().Code)
	attrs := attrMap(okSpan.Attributes())
	require.Equal(t, "postgresql", attrs["db.system"].AsString())
	require.Equal(t, "appdb", attrs["db.namespace"].AsString())
	require.Equal(t, "orders.list", attrs["tabula.key"].AsString())
	require.Equal(t, "orders", attrs["tabula.table"].AsString())
	require.Equal(t, "list", attrs["tabula.action"].AsString())
	require.Equal(t, "app_reader", attrs["tabula.role"].AsString())
	require.Equal(t, "t-1", attrs["tabula.scope"].AsString())
	require.Equal(t, int64(4), attrs["tabula.rows"].AsInt64())

	errSpan := spans[1]
	require.Equal(t, "tabula.statement", errSpan.Name())
	require.Equal(t, codes.Error, errSpan.Status().Code)
	require.Equal(t, string(tabula.ClassUnique), errSpan.Status().Description)
	errAttrs := attrMap(errSpan.Attributes())
	require.Equal(t, string(tabula.ClassUnique), errAttrs["tabula.error_class"].AsString())
	require.Equal(t, "23505", errAttrs["db.response.status_code"].AsString())
	require.NotEmpty(t, errSpan.Events(), "RecordError must attach an exception event")

	// Nil provider falls back to global provider cleanly.
	_, endDefault := oteltabula.NewTracer(nil).Start(t.Context(), info)
	endDefault(-1, nil)
}

func TestMetricsRecordsHistogramAndCounter(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	m, err := oteltabula.NewMetrics(mp,
		oteltabula.WithInstrumentationVersion("0.3.1"),
		oteltabula.WithAttributes(attribute.String("db.namespace", "appdb")),
	)
	require.NoError(t, err)

	info := tabula.TraceInfo{
		Key:    "tabula.inline.abcdef012345",
		Action: "select",
		Role:   "app_reader",
		Scope:  "t-1",
	}
	m.Observe(t.Context(), info, 25*time.Millisecond, 3, "")
	m.Observe(t.Context(), info, 50*time.Millisecond, 0, tabula.ClassRLSDenied)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Len(t, rm.ScopeMetrics, 1)

	metricsByName := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics[0].Metrics {
		metricsByName[sm.Name] = sm
	}
	require.Contains(t, metricsByName, "tabula.statement.duration")
	require.Contains(t, metricsByName, "tabula.statement.count")

	hist, ok := metricsByName["tabula.statement.duration"].Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, hist.DataPoints, 2)

	sum, ok := metricsByName["tabula.statement.count"].Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, sum.DataPoints, 2)
	var total int64
	var sawRLS bool
	for _, dp := range sum.DataPoints {
		total += dp.Value
		if v, exists := dp.Attributes.Value("tabula.error_class"); exists && v.AsString() == string(tabula.ClassRLSDenied) {
			sawRLS = true
		}
	}
	require.Equal(t, int64(2), total)
	require.True(t, sawRLS)

	// Works end-to-end through tabula.Observer.
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	obs := tabula.NewObserver(tabula.ObserverConfig{
		Role:    "app_writer",
		Tracer:  oteltabula.NewTracer(tp),
		Metrics: m,
	})
	opCtx, done := obs.Start(t.Context(), "t-2", "SELECT 1", nil)
	done(1, errors.New("boom"))
	require.NotNil(t, opCtx)
	require.Len(t, sr.Ended(), 1)
	require.Equal(t, codes.Error, sr.Ended()[0].Status().Code)

	// Nil meter provider falls back cleanly.
	_, err = oteltabula.NewMetrics(nil)
	require.NoError(t, err)
}

func attrMap(attrs []attribute.KeyValue) map[attribute.Key]attribute.Value {
	out := make(map[attribute.Key]attribute.Value, len(attrs))
	for _, a := range attrs {
		out[a.Key] = a.Value
	}
	return out
}
