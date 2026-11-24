# Logging specification

Tabula logs only when the application asks it to, through a logger the application
passes in. This keeps the third red line of [DESIGN.md](DESIGN.md): no global state and
no hidden behaviour.

## 1. Where the logger comes from

- The library uses the standard library `log/slog`. The core takes no third-party logging
  dependency.
- A logger is passed explicitly in `RegistryConfig.Log` (applies to every table and view of
  that registry) or in `RunnerOptions.Log` (applies to every statement on a runner's session,
  including hand-written inline SQL), and may be overridden per session binding via
  `WithLogger`. There is no package-level logger and no `slog.Default()` fallback.
- When both a runner and a registry configure telemetry, each enabled signal (tracing,
  metrics, structured logging) has **one owner**: the runner takes precedence for signals it
  handles and the registry supplies any remaining signals. Per-binding `WithLogger`
  overrides and `Secret()` / `db:"...,secret"` / `QueryFilter.Secret` redaction are preserved
  when the runner owns logging.
- Statement logs require an effective logger and a level other than `LogOff`. Active logging or a metrics hook enables duration measurement using `LogConfig.Now` (default `time.Now`).

```go
reg := tabula.NewRegistry(tabula.RegistryConfig[Scope]{
    // ...
    Log: tabula.LogConfig{
        Logger:        logger,                 // *slog.Logger; nil disables logging
        Level:         tabula.LogErrors,      // switch, see section 2
        SlowThreshold: 200 * time.Millisecond, // 0 disables slow-statement entries
        Values:        tabula.ValuesNever,    // see section 4
    },
})
```

## 2. Switches

| `Level` | What is written |
| --- | --- |
| `LogOff` (default) | Nothing |
| `LogErrors` | Failed statements only |
| `LogSlow` | Failed statements and statements slower than `SlowThreshold` |
| `LogAll` | Every statement (debugging; not for production traffic) |

- The level can be changed at run time through an `atomic`-backed `LogLevelVar.Set` (passed via `LogConfig.LevelVar`) or `Registry.SetLogLevel`, so
  an operator can raise it temporarily without a restart.
- `Sample` (0–1) applies to `LogAll` only; errors and slow statements are never sampled.
- A per-statement override is available at registration: `Statement(key, sql,
  tabula.Quiet())` for a statement that must never be logged above `LogErrors` (for
  example a heartbeat).

## 3. Record shape

Every entry is one structured record with fixed keys, so logs can be queried the same way
in every application.

| Key | Type | Meaning |
| --- | --- | --- |
| `msg` | string | `tabula.statement`, `tabula.slow_statement` or `tabula.statement_failed` |
| `tabula.key` | string | Registered statement key (for example `catalog.items_by_code`); derived statements use `<table>.<action>` |
| `tabula.table` | string | Table or view name |
| `tabula.action` | string | `get`, `lock`, `insert`, `save`, `list`, `count`, `view`, `exec`, ... |
| `tabula.duration_ms` | float | Elapsed time |
| `tabula.rows` | int | Rows returned or affected, when known |
| `tabula.error_class` | string | `ErrorClass` returned by `ClassifyError` on failure. [Class constants](REFERENCE.md#tabula) |
| `tabula.error` | string | Driver error message or the result of `ErrorPolicy.Sanitize` on failure |
| `tabula.args` | list of strings | Bound arguments when the value policy includes them. See section 4. |
| `tabula.sql` | string | SQL text when `Level` is `LogAll` and `LogConfig.SQL` is enabled |
| `tabula.sqlstate` | string | PostgreSQL SQLSTATE, when the driver exposes it |
| `tabula.role` | string | Runner PostgreSQL role when configured on `RunnerOptions.Role` |
| `tabula.scope` | string | Only what the application's `ScopeLog` function returns (for example a tenant ID); omitted when not configured |

- Request IDs, trace IDs and user IDs are not added by the library. Use a `slog.Handler`
  that reads them from the context; Tabula always calls the logger with the statement's
  `context.Context`.

## 4. Values and sensitive data

- `ValuesNever` (default): argument values and row contents are never logged.
- `ValuesRedacted`: arguments are logged with their Go type and length only.
- `ValuesAllowed`: arguments are logged, except columns tagged `secret`
  (`db:"password_hash,secret"`) and arguments of statements registered with
  `tabula.Secret()`, which are always replaced by `"[redacted]"`.
- Error messages from the driver are logged as returned; the `ErrorPolicy` can supply a
  `Sanitize` function when the database may echo values in error details.

## 5. Tracing and metrics

Logging, tracing and metrics share the same statement key, role and action, and can be
configured on `RegistryConfig` or on `RunnerOptions` (`Tracer`, `Metrics`, `Log`, `Sanitize`,
`TagStatements`). When configured on a runner, hand-written inline SQL receives the key
`tabula.inline.<fingerprint>` derived from normalized SQL text (never bound argument values).

The separate OpenTelemetry module `github.com/0xJean/tabula/oteltabula` implements
`tabula.Tracer` (`oteltabula.NewTracer`) and `tabula.Metrics` (`oteltabula.NewMetrics`)
over `go.opentelemetry.io/otel` without adding third-party dependencies to core packages.
Spans carry full statement context (`tabula.key`, `tabula.table`, `tabula.action`,
`tabula.role`, `tabula.scope`, `tabula.rows`, `tabula.error_class`), while metrics emit
bounded-cardinality labels (`db.system`, `tabula.table`, `tabula.action`, `tabula.role`,
`tabula.error_class`) and omit per-tenant `tabula.scope` and statement/inline-fingerprint
`tabula.key` values. See the [Go reference](REFERENCE.md#oteltabula).

## 6. Database-side slow statement logs

Client-side `LogSlow` captures Go-observed latency including connection checkout and network
round trips, while PostgreSQL's own server logs and statistics capture planner and executor
behavior directly:

- **`log_min_duration_statement`**: set per role or database (for example `200ms`) so
  PostgreSQL logs slow server-side executions with bind parameters controlled by
  `log_parameter_max_length`.
- **`auto_explain`**: load `auto_explain` with `auto_explain.log_min_duration` (and
  `auto_explain.log_analyze = off` in production hot paths unless diagnosing cardinality) to
  capture execution plans alongside slow statements.
- **`pg_stat_statements`**: tracks normalized execution counts, mean/max time, shared buffer
  hits/reads and temp spills across restarts and connections.

### Correlating server logs with Tabula statement keys

1. **Per-service `application_name`**: set `application_name` in the connection DSN or
   `pgxpool.Config` (for example `billing-api:app_reader`) and include `%a` in
   `log_line_prefix` to separate services and caller roles in PostgreSQL logs.
2. **Opt-in SQL comment tagging (`RunnerOptions.TagStatements: true`)**: prefixes each
   executed statement with `/* tabula:<key> */ ` (using the registered statement key for
   descriptor and composed-query statements, or `tabula.inline.<fingerprint>` for inline SQL;
   comment delimiters and newlines in keys are sanitized). Comments appear verbatim in
   `log_min_duration_statement`, `auto_explain` and `pg_stat_activity`, while
   `pg_stat_statements` preserves or normalizes them per PostgreSQL configuration. Keep
   `TagStatements` opt-in when `pg_stat_statements` cardinality per query shape is tightly
   budgeted.

## 7. Tests the library keeps

- Logging off: no call reaches the handler.
- Each level writes exactly the entries listed in section 2.
- Default `ValuesNever` and `secret` columns never appear in output, checked by a test
  handler that fails on any argument value.
- Record keys and types match section 3 (golden test).
