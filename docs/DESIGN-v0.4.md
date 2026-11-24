# Tabula v0.4 design: guarded runners, session observability, query composition

Status: proposed. This document plans the v0.4 work; nothing here is implemented yet.
The red lines in [DESIGN.md](DESIGN.md) and [AGENTS.md](../AGENTS.md) apply unchanged:
no generated joins, no DDL, no hidden global state, strict domain neutrality, and no
third-party dependencies in core packages.

## 0. Why

v0.3 covers descriptors, single-table writes and the full authorization and RLS
verification toolkit ([AUTHORIZATION.md](AUTHORIZATION.md)). Applications that enforce
row-level security with per-caller PostgreSQL roles still have to write and maintain
their own transaction runner around Tabula: pinning a role, setting timeouts, sharing
one transaction between a scoped and an unscoped session, guarding sessions, tracing
every statement and retrying serialization failures. They also keep hand-writing the
boilerplate around joins: optional filters, sort whitelists, pagination and small
aggregates. v0.4 moves these generic parts into Tabula so that an application keeps only
its configuration and business SQL.

| Group | Scope |
| --- | --- |
| A | Role-bound runners and guarded sessions (`stdsql`, `pgxdriver`, `tabula`) |
| B | Session-level observability and an OpenTelemetry module |
| C | Query composition and single-table aggregates |
| D | Verification support for groups A and C |

## A. Role-bound runners and guarded sessions

### A1. Role-bound runners

A runner is bound to one PostgreSQL role when it is constructed. The role never follows
request content.

```go
runner, err := stdsql.NewRunner(db, policy, stdsql.RunnerOptions{
    Role:     "app_writer",            // SET LOCAL ROLE after BEGIN; required when set
    ReadOnly: false,                    // true: Within fails closed with ErrReadOnly
    Unscoped: stdsql.UnscopedReadOnly,  // UnscopedDenied | UnscopedReadOnly | UnscopedReadWrite
    Timeouts: stdsql.Timeouts{Lock: time.Second, Statement: 5 * time.Second, IdleInTransaction: 10 * time.Second},
})
```

- `NewRunner` returns an error (not a panic) for an empty role with `RequireRole`, a
  read-only runner asked for writes, or an unscoped write on a runner whose `Unscoped`
  forbids it. These are construction-time checks; the same checks run again per call.
- Order inside each transaction, fixed and documented: `SET LOCAL ROLE`, the three
  timeouts with `set_config(..., true)`, the scope variables from `ScopePolicy`, then the
  context variables from A2. Everything is transaction-local and disappears at COMMIT or
  ROLLBACK, so a pooled connection returns to the login role.
- Timeouts use `set_config` because `ALTER ROLE ... SET` is not applied after `SET ROLE`.

### A2. Context-derived variables

```go
policy := stdsql.ScopePolicy[Scope]{
    ScopeVars:   func(s Scope) map[string]string { return map[string]string{"example.scope": s.ID()} },
    ContextVars: func(ctx context.Context) map[string]string { return map[string]string{"example.request_id": requestID(ctx)} },
    IsUnscoped:  func(s Scope) bool { return s.All() },
    UnscopedVars: map[string]string{"example.scope_mode": "all"},
}
```

Variable names are supplied by the application. Tabula validates them at construction
(non-empty, dotted custom GUC names) and protects them with `NewSessionGuard`
automatically: adapter SQL cannot read-modify them through `set_config`, `SET`, `RESET`
or `DISCARD`.

### A3. Scoped and unscoped sessions in one transaction

```go
err := runner.WithinShared(ctx, func(ctx context.Context, tx *stdsql.Shared[Scope]) error {
    all := tx.Unscoped()     // unscoped session, shares the transaction
    one := tx.Scoped(scope)  // scoped session, shares the transaction
    ...
})
```

- One transaction, one role. Switching between sessions re-applies the scope variables
  with `set_config(..., true)` before the statement, under a mutex; sessions are not safe
  for concurrent use and become invalid after the callback returns.
- `Unscoped` obeys the runner's `Unscoped` mode from A1.
- Use case: a background job reads shared configuration unscoped, then writes per-scope
  rows in the same transaction.

### A4. Sealed sessions

`Bind` currently accepts any `tabula.Session[S]` implementation, so a registry cannot tell
a guarded session from an arbitrary one. v0.4 adds an optional session key.

```go
key := tabula.NewSessionKey()                       // created once by the application's assembly
reg := tabula.NewRegistry(tabula.RegistryConfig[Scope]{SessionKey: key, ...})
runner, _ := stdsql.NewRunner(db, policy, stdsql.RunnerOptions{SessionKey: key, ...})
```

- With `SessionKey` set, `Table.Bind` and `View.Bind` reject sessions that were not
  created by a runner holding the same key (`ErrForeignSession`). The key is an opaque
  value; it cannot be derived from a session.
- `stdsql` and `pgxdriver` runners attach the key; a custom driver attaches it through
  the exported `tabula.Seal(session, key)` so that `pgxdriver` stays a public-API-only
  module.
- Effect: adapters may import `tabula` directly; only the code that holds the key (the
  application's assembly) can create sessions a registry will accept.

### A5. No raw transaction escape

`stdsql.Session.Tx()` exposes the underlying `*sql.Tx` and bypasses every guard. v0.4
removes it from the default session. Applications that need it construct the runner with
`RunnerOptions{ExposeTx: true}`; `Tx()` then exists on a separate `RawSession` type so its
use is visible in code review and in `tabula inventory`.

### A6. Registry capability policy

```go
reg := tabula.NewRegistry(tabula.RegistryConfig[Scope]{
    Deny: tabula.DenyDelete | tabula.DenyUnscopedWrites,
})
```

- `Define` and `DefineUpsert` fail when a spec uses a denied capability, for example
  `TableSpec.AllowDelete` under `DenyDelete`. Applications no longer re-check spec fields
  in a wrapper, and new spec fields in later releases are covered by a new `Deny` bit.
- Initial bits: `DenyDelete`, `DenyUpsert`, `DenyUnscopedWrites`, `DenyCopy`.
- Default `Deny` is zero to keep v0.3 behaviour.

### A7. Transaction-level retry

```go
err := runner.WithinRetry(ctx, scope, stdsql.Retry{
    MaxAttempts: 3,
    Backoff:     tabula.ExponentialBackoff(10*time.Millisecond, 200*time.Millisecond),
    Retryable:   tabula.IsRetryable, // default classifier: 40001, 40P01
}, fn)
```

- The whole callback runs again in a new transaction; single statements are never
  retried inside a failed transaction.
- No retry unless the caller uses `WithinRetry`. A caller can supply its own classifier,
  for example to treat a serialization failure as a final business outcome.
- The callback must be idempotent apart from the database; GoDoc states this.

### A8. Login role hygiene check

`sqlcheck.CheckLoginRole(ctx, db, sqlcheck.LoginRoleExpectation{...})` verifies that a
login role is `LOGIN NOINHERIT`, has no superuser, BYPASSRLS, CREATEROLE, CREATEDB or
REPLICATION attribute, is a member of exactly the expected roles, holds no direct table
privileges, owns no objects and cannot create in the selected schemas. The CLI gains
`tabula check-login-role`.

## B. Session-level observability

### B1. Hooks on runners and sessions

v0.3 attaches `Tracer`, `Metrics` and the logger to `RegistryConfig`, so only descriptor
operations are observed. v0.4 adds the same interfaces to `RunnerOptions`; every
`Exec`, `Scan`, `Returning` and `Each` on a runner's session is observed, including
hand-written SQL.

- Span and metric names: the registered statement key when known; otherwise
  `tabula.inline` with a stable fingerprint of the normalized SQL text, never argument
  values.
- Attributes: `db.system=postgresql`, role, action (`select`, `insert`, ...), rows,
  error class (v0.3 classes plus `rls_denied` and `insufficient_privilege`), and the
  application's `ScopeLog` label when configured.
- Registry-level hooks remain for applications that bring their own sessions; when both
  are configured, the runner hook wins and the registry hook is skipped for that call.

### B2. OpenTelemetry module

`github.com/0xJean/tabula/otel` is a separate Go module (like `pgxdriver`) implementing
`Tracer` and `Metrics` over the OpenTelemetry API. Core packages stay standard-library
only. This moves the OpenTelemetry item from v1.0 to v0.4.

### B3. Database-side slow statement logs

[LOGGING.md](LOGGING.md) gains a section on server-side logging
(`log_min_duration_statement`, `auto_explain`, `pg_stat_statements`) and how statement
keys from B1 can be correlated with them through `application_name` or SQL comments
(opt-in `RunnerOptions.TagStatements`, which prefixes `/* tabula:<key> */`).

## C. Query composition and single-table aggregates

The red line stays: Tabula never generates a join, never reads relations from struct
tags and never preloads. The application writes the join; Tabula manages the parts
around it.

### C1. Single-table aggregates

```go
n, err := rows.Max(ctx, "sort_order", filter)          // also Min, Sum, Avg
ok, err := rows.Exists(ctx, filter)
counts, err := rows.CountBy(ctx, "status", filter)      // map[value]int64
```

- Columns must be declared on the table and, for `CountBy`, whitelisted on the spec
  (`TableSpec.GroupBy`). Scope and soft delete apply as for `Find`.
- Each aggregate is rendered at `Define` time per column and recorded in `Definition`,
  so it appears in snapshots and in `verify-roles` workloads.

### C2. Composed view queries

```go
var orders = tabula.DefineQuery(reg, tabula.QuerySpec[OrderRow]{
    Key:  "orders.list",
    Base: `SELECT ` + orderView.ColsAs("o") + `, c.name AS customer_name
           FROM shop.orders o JOIN shop.customers c ON c.id = o.customer_id
           WHERE o.archived = false`,
    Filters: map[string]string{
        "status":   "o.status = ?",
        "customer": "o.customer_id = ?",
        "since":    "o.created_at >= ?",
        "search":   "c.name ILIKE ?",
    },
    Order: tabula.MustOrderSpec(tabula.OrderSpec{ /* whitelisted sort keys */ }),
    Page:  tabula.PageOffset | tabula.PageKeyset,
})

page, err := orders.Bind(sess).Run(ctx, tabula.QueryArgs{
    Filters: map[string]any{"status": "paid", "since": since},
    Sort:    "created_at", Desc: true, Limit: 50, After: cursor,
})
```

- The base SQL and every filter fragment are fixed at definition time; callers choose
  filters by name and supply values only. `?` placeholders are renumbered to `$n`.
- Fragments are appended with `AND`; the base must contain a top-level `WHERE` (or
  Tabula adds `WHERE TRUE`). Validation at `Define` rejects fragments containing
  statement separators or session-changing SQL (`NewSessionGuard` rules).
- Keyset pagination requires a unique tie-breaker in the `OrderSpec`.
- Counting follows v0.3 count strategies.

### C3. Verifying composed queries

A composed query has up to 2^n filter combinations. `Definition` records a canonical
variant with every filter enabled plus every single-filter variant; snapshots,
`verify-roles` workloads and `check-coverage` use these. Because fragments only add
`AND` predicates, the canonical variant references every column and table any variant
can reference.

### C4. Embedded SQL files

```go
//go:embed sql/*.sql
var files embed.FS
var sqlFiles = tabula.MustSQLFiles(files, "sql")
view.Statement("orders.report", sqlFiles.Get("orders_report.sql"))
```

Long joins live in `.sql` files, are loaded once at init and are registered like any
other statement. Missing files fail at start-up.

## D. Verification support

- `tabula inventory` recognizes `DefineQuery` and aggregate calls as static sites.
- The workload format accepts composed-query variants from C3 without manual listing.
- A4 sessions and A5 `RawSession` are reported by `tabula inventory` so that raw
  transaction use is reviewable.

## Compatibility

| Change | Impact |
| --- | --- |
| A5 removes `stdsql.Session.Tx()` from the default session | Breaking for callers of `Tx()`; migrate to `ExposeTx` and `RawSession` |
| B1 runner hooks take precedence over registry hooks | Behaviour change only when both are configured |
| A1, A3, A4, A6, A7, A8, C1–C4 | Additive |

[MIGRATION.md](MIGRATION.md) gains a v0.3 → v0.4 section in the release commit.

## Out of scope

Generated joins, relations in struct tags, preloading, query builders for arbitrary SQL,
role or policy creation, migrations, and any application-specific role names or
variables.

## Delivery order and acceptance

1. A4, A5, A6 (sealed sessions, raw escape, capability policy): small, unblock adapters
   importing `tabula` directly.
2. A1, A2, A3, A7, A8 (runner): replace hand-written runners.
3. B1, B2, B3 (observability).
4. C1, C2, C3, C4 and D (composition).

Every item ships with unit tests and real PostgreSQL integration tests (`-tags=integration
-race`, PostgreSQL 14–17), fuzz seeds for any new SQL text handling (C2 fragments, A4
key handling has none), GoDoc, and updates to README, CHANGELOG, COOKBOOK, MIGRATION and
REFERENCE in the same commit, per [AGENTS.md](../AGENTS.md).
