//go:build integration

// Package integration_test exercises the released adapters together in the Go workspace.
package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/oteltabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/stdsql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func databases(t *testing.T) (*sql.DB, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return db, pool
}

// Inspect the public hook before the OpenTelemetry adapter receives it.
type checkingTracer struct {
	tabula.Tracer
	t     *testing.T
	count int
	raw   bool
}

func (p *checkingTracer) Start(ctx context.Context, info tabula.TraceInfo) (context.Context, func(int64, error)) {
	ctx, finish := p.Tracer.Start(ctx, info)
	return ctx, func(rows int64, err error) {
		p.count++
		class, _ := tabula.ClassifyError(ctx, err)
		require.Equal(p.t, p.raw, info.RawErrorDiagnostics)
		if !p.raw {
			require.EqualError(p.t, err, string(class))
		}
		require.NotEmpty(p.t, tabula.SQLState(err))
		_, pqCause := errors.AsType[*pq.Error](err)
		_, pgxCause := errors.AsType[*pgconn.PgError](err)
		require.Equal(p.t, p.raw, pqCause || pgxCause)
		finish(rows, err)
	}
}

func TestDatabaseErrorTelemetry(t *testing.T) {
	db, pool := databases(t)
	ctx := t.Context()
	table := fmt.Sprintf("tabula_diag_%d", time.Now().UnixNano())
	_, err := db.ExecContext(ctx, "CREATE TABLE "+table+" (label text UNIQUE, short varchar(2))")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE " + table) })
	cases := []struct{ state, marker, query string }{
		{"22P02", "probe_value_uuid_493", "SELECT $1::text::uuid"},
		{"22003", "98765432109876543210987654321", "SELECT $1::text::bigint"},
		{"22001", "probe_value_length_851", "INSERT INTO " + table + "(short) VALUES ($1) RETURNING short"},
		{"23505", "probe_value_unique_624", "INSERT INTO " + table + "(label) VALUES ($1) RETURNING label"},
	}
	_, err = db.ExecContext(ctx, "INSERT INTO "+table+"(label) VALUES ($1)", cases[3].marker)
	require.NoError(t, err)

	for _, driver := range []string{"pq", "pgx"} {
		for _, source := range []string{"runner", "registry"} {
			for _, mode := range []struct{ raw, reviewed bool }{{}, {reviewed: true}, {raw: true}, {raw: true, reviewed: true}} {
				t.Run(fmt.Sprintf("%s/%s/raw=%t/sanitize=%t", driver, source, mode.raw, mode.reviewed), func(t *testing.T) {
					var logs bytes.Buffer
					exporter := tracetest.NewInMemoryExporter()
					tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
					reader := sdkmetric.NewManualReader()
					mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
					t.Cleanup(func() { _ = tp.Shutdown(context.Background()); _ = mp.Shutdown(context.Background()) })
					metrics, err := oteltabula.NewMetrics(mp)
					require.NoError(t, err)
					tracer := &checkingTracer{Tracer: oteltabula.NewTracer(tp), t: t, raw: mode.raw}
					cfg := tabula.RegistryConfig[struct{}]{
						Tracer: tracer, Metrics: metrics, RawErrorDiagnostics: mode.raw,
						Log: tabula.LogConfig{Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Level: tabula.LogErrors, Values: tabula.ValuesNever},
					}
					if mode.reviewed {
						cfg.Errors.Sanitize = func(err error) string { return "reviewed " + tabula.SQLState(err) }
					}
					regCfg, runnerCfg := cfg, tabula.RegistryConfig[struct{}]{}
					if source == "runner" {
						regCfg, runnerCfg = runnerCfg, regCfg
					}
					type projection struct {
						Value string `db:"value,zero"`
					}
					view := tabula.DefineView(tabula.NewRegistry(regCfg), tabula.ViewSpec[projection]{Name: table})
					std, err := stdsql.NewRunner(db, stdsql.NoScopePolicy(), stdsql.RunnerOptions{
						Tracer: runnerCfg.Tracer, Metrics: runnerCfg.Metrics, Log: runnerCfg.Log, Sanitize: runnerCfg.Errors.Sanitize, RawErrorDiagnostics: runnerCfg.RawErrorDiagnostics,
					})
					require.NoError(t, err)
					native, err := pgxdriver.NewRunner(pool, pgxdriver.NoScopePolicy(), pgxdriver.RunnerOptions{
						Tracer: runnerCfg.Tracer, Metrics: runnerCfg.Metrics, Log: runnerCfg.Log, Sanitize: runnerCfg.Errors.Sanitize, RawErrorDiagnostics: runnerCfg.RawErrorDiagnostics,
					})
					require.NoError(t, err)
					var driverMessages []string
					for _, tc := range cases {
						run := func(ctx context.Context, s tabula.Session[struct{}]) error {
							var value string
							if source == "runner" {
								return s.Scan(ctx, tc.query, []any{tc.marker}, &value)
							}
							return view.Bind(s).Scan(ctx, tc.query, []any{tc.marker}, &value)
						}
						if driver == "pgx" {
							err = native.Within(ctx, struct{}{}, func(ctx context.Context, s *pgxdriver.Session[struct{}]) error { return run(ctx, s) })
						} else {
							err = std.Within(ctx, struct{}{}, func(ctx context.Context, s *stdsql.Session[struct{}]) error { return run(ctx, s) })
						}
						require.Error(t, err)
						require.Equal(t, tc.state, tabula.SQLState(err), "%v", err)
						if driver == "pgx" {
							cause, ok := errors.AsType[*pgconn.PgError](err)
							require.True(t, ok)
							driverMessages = append(driverMessages, cause.Error())
						} else {
							cause, ok := errors.AsType[*pq.Error](err)
							require.True(t, ok)
							driverMessages = append(driverMessages, cause.Error())
						}
					}
					require.Equal(t, len(cases), tracer.count)
					spans := exporter.GetSpans()
					require.Len(t, spans, len(cases))
					records := strings.Split(strings.TrimSpace(logs.String()), "\n")
					require.Len(t, records, len(cases))
					for i, s := range spans {
						class := string(tabula.ClassOther)
						if cases[i].state == "23505" {
							class = string(tabula.ClassUnique)
						}
						require.Equal(t, codes.Error, s.Status.Code)

						attrs := map[string]string{}
						for _, attr := range s.Attributes {
							attrs[string(attr.Key)] = attr.Value.AsString()
						}
						require.Equal(t, cases[i].state, attrs["db.response.status_code"])
						require.Equal(t, class, attrs["tabula.error_class"])
						require.Len(t, s.Events, 1)
						require.Equal(t, "exception", s.Events[0].Name)
						if mode.raw {
							require.Equal(t, driverMessages[i], s.Status.Description)
							event := map[string]string{}
							for _, attr := range s.Events[0].Attributes {
								event[string(attr.Key)] = attr.Value.AsString()
							}
							require.Equal(t, driverMessages[i], event["exception.message"])
							if cases[i].state == "22P02" || cases[i].state == "22003" {
								require.Contains(t, event["exception.message"], cases[i].marker)
							}
						} else {
							require.Equal(t, class, s.Status.Description)
							require.Len(t, s.Events[0].Attributes, 1)
							require.Equal(t, class, s.Events[0].Attributes[0].Value.AsString())
						}
						var record map[string]any
						require.NoError(t, json.Unmarshal([]byte(records[i]), &record))
						require.Equal(t, cases[i].state, record["tabula.sqlstate"])
						want := class
						if mode.reviewed {
							want = "reviewed " + cases[i].state
						}
						require.Equal(t, want, record["tabula.error"])
					}
					var rm metricdata.ResourceMetrics
					require.NoError(t, reader.Collect(t.Context(), &rm))
					require.Len(t, rm.ScopeMetrics, 1)
					require.Len(t, rm.ScopeMetrics[0].Metrics, 2)
					protected := []any{rm, logs.String()}
					if !mode.raw {
						protected = append(protected, spans)
					}
					for _, signal := range protected {
						data, err := json.Marshal(signal)
						require.NoError(t, err)
						for _, tc := range cases {
							require.NotContains(t, string(data), tc.marker)
						}
					}
				})
			}
		}
	}
}
