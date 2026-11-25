# Statement diagnostics, role readiness and callback contexts

## Error diagnostics

By default, both runner observers and descriptor registries pass a bounded error to the finish callback of `Tracer.Start`. The existing tracer interface stays source-compatible. On failure, its error text is the Tabula error class. `SQLState` and `ClassifyError` recover the five-character PostgreSQL code and the original classification. The diagnostic retains no driver error, detail, cause or query argument. The database operation still returns its original error chain.

The OpenTelemetry bridge keeps the error status, class, optional SQLSTATE and an exception event carrying the class. The same default applies to direct calls to the bridge. Set `RawErrorDiagnostics: true` on the runner or registry that owns the trace to pass the original error to the hook. The bridge then records its text in the exception event and status description. This can expose rejected parameter values, for example in SQLSTATE 22P02 or 22003. `TraceInfo.RawErrorDiagnostics` communicates the choice to tracing adapters. Metrics receive the existing bounded error class. The default structured log field `tabula.error` contains the class. An explicitly supplied runner `Sanitize` or registry `ErrorPolicy.Sanitize` may supply application-reviewed text for this log field. SQL and argument logging remain independent opt-ins. Applications migrating custom tracers should inspect the class and SQLSTATE in the hook and inspect driver details at the call site.

## Role readiness

```go
type ReadinessNeeds struct {
    Privileges      PrivilegeNeeds
    SchemaUsage     []string
    AnyColumnSelect []string
    ForceRLS        []string
    SequenceUsage   []string
}

type MissingRequirement struct {
    Kind, Object, Column, Privilege string
}

func CheckReadiness(ctx context.Context, db *sql.DB, role string, needs ...ReadinessNeeds) ([]MissingRequirement, error)
```

Supply the actual role name explicitly. Pass the output of `CollectPrivilegeNeeds` in one group and hand-written requirements in others. One parameterized catalog query checks their union in one statement snapshot. It uses PostgreSQL's effective privilege functions, including privileges granted through membership, ownership, table-level grants and PUBLIC. It reads no application rows, switches no roles and never invokes the requested routines. Functions use complete signatures so that overloads remain distinct. Use schema-qualified relation and function names, plain catalog names for schemas and columns, and qualify application-defined argument types.

`AnyColumnSelect` accepts a role that can read even one column. `ForceRLS` requires both ENABLE and FORCE on an ordinary or partitioned table, independently of read access. Descriptor sequences identify the sequence owned by a table column, including identity columns. `SequenceUsage` also accepts named sequences. Results identify each missing requirement once in deterministic order. A missing role produces one ROLE/EXISTS finding. A missing object or grant produces the requirements that could not be met. Invalid configuration, cancellation and database failures return an error. The caller owns timeouts and the readiness decision. Use the existing grant and policy audits for excess privileges and policy semantics.

## Callback ownership

`stdsql.RunnerOptions.RequireCallbackContext` and `pgxdriver.RunnerOptions.RequireCallbackContext` default to false. With the option enabled, a runner session accepts the context supplied to its callback and contexts derived from it. A live session paired with a different callback or parent context returns `tabula.ErrCallbackContext` before database I/O. A closed handle retains `ErrSessionClosed`. The driver helper `tabula.BindCallbackContext(ctx)` returns a marked context and a check function backed by a fresh private identity. The check retains only that identity.

Each attempt and each `BeforeCommit` step gets its own identity. Scoped and unscoped handles in a shared callback use one identity. Savepoints belong to the enclosing runner callback in both adapters, including the native pgx child session. Raw session forwarding methods and binary COPY check ownership. Direct sessions have no callback. Explicit driver access through `RawSession.Tx()` remains the caller's responsibility. `WithoutTransaction` detaches callback ownership when starting independent work. Database roles, scope equality, seals and callback ownership answer separate questions.
