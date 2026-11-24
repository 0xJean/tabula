# Logging specification

Tabula logs only when the application asks it to, through a logger the application
passes in. This keeps the third red line of [DESIGN.md](DESIGN.md): no global state and
no hidden behaviour.

## 1. Where the logger comes from

- The library uses the standard library `log/slog`. The core takes no third-party logging
  dependency.
- A logger is passed explicitly in `RegistryConfig.Log` (applies to every table and view of
  that registry) and may be overridden per session binding. There is no package-level
  logger and no `slog.Default()` fallback.
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

Logging, tracing and metrics share the same statement key and action, but are separate
interfaces passed in the same way (`RegistryConfig.Tracer`, `RegistryConfig.Metrics`).
In v0.3 the application implements these interfaces to send observations to its telemetry backend. The [Go reference](REFERENCE.md#tabula) gives their method signatures. Optional OpenTelemetry and Prometheus adapters are listed in the [roadmap](../ROADMAP.md).

## 6. Tests the library keeps

- Logging off: no call reaches the handler.
- Each level writes exactly the entries listed in section 2.
- Default `ValuesNever` and `secret` columns never appear in output, checked by a test
  handler that fails on any argument value.
- Record keys and types match section 3 (golden test).
