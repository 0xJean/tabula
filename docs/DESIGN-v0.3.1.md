# Tabula v0.3.1 design: guarded runners, session observability, query composition

**Status: Implemented in v0.3.1.** All groups (A1–A8, B1–B3, C1–C4, D) have been
implemented with unit tests, real PostgreSQL integration tests and
`FuzzCompileQueryFragment`, and the proposal-only `docs/internal/v04` test package has
been removed prior to tagging. For the shipped API use the
[v0.3 integration guide](INTEGRATION.md) and [API reference](REFERENCE.md). The
project constraints are in [DESIGN.md](DESIGN.md) and [AGENTS.md](../AGENTS.md).

## 0. Why

v0.3.0 covers descriptors, single-table writes and the full authorization and RLS
verification toolkit ([AUTHORIZATION.md](AUTHORIZATION.md)). Applications that enforce
row-level security with per-caller PostgreSQL roles still have to write and maintain
their own transaction runner around Tabula: pinning a role, setting timeouts, sharing
one transaction between a scoped and an unscoped session, guarding sessions, tracing
every statement and retrying serialization failures. They also keep hand-writing the
boilerplate around joins: optional filters, sort whitelists, pagination and small
aggregates. v0.3.1 moves these generic parts into Tabula so that an application keeps only
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
    Role:     "app_writer",
    ReadOnly: false,                    // true: Within fails closed with ErrReadOnly
    Unscoped: stdsql.UnscopedReadOnly,  // UnscopedDenied | UnscopedReadOnly | UnscopedReadWrite
    Timeouts: stdsql.Timeouts{Lock: time.Second, Statement: 5 * time.Second,
        IdleInTransaction: 10 * time.Second, Transaction: 30 * time.Second},
})
```

- Construction checks the role requirement, timeout bounds and mode configuration.
  Every entry point checks the requested transaction mode **before BEGIN**, including
  driver-specific options. A read-only runner returns `ErrReadOnly` on a read-write
  request even with explicit options. Unscoped reads and writes obey the runner's mode.
- `Timeouts.Transaction` sets a Go deadline before BEGIN for the whole attempt.
  Pass it to SQL and callbacks and check it before commit. Callback work must
  observe cancellation. Zero inherits the caller's context. An earlier caller
  deadline wins. The three server timeouts bound individual waits, statements and idle
  periods. After BEGIN, issue the quoted `SET LOCAL ROLE` when configured, then one
  parameterized SELECT of transaction-local `set_config` calls for all timeouts and
  scope/context variables. For read-only transactions this batch also sets
  `transaction_read_only` and takes the first snapshot before user SQL runs.
  Validate the complete set and reject duplicate names first.
  This takes at most two setup round trips. Setup failure rolls back before user code.
- Pass a marked context to the callback. A nested runner entry using that context
  (including another runner) returns `ErrNested` before BEGIN. Use a savepoint for
  nested work. This check follows context propagation. Callers that deliberately open
  an independent transaction from inside a callback (for example, an out-of-band audit
  write that must commit even if the outer transaction rolls back) must opt in with
  an explicit root-package helper (`tabula.WithoutTransaction(ctx)`) that clears only
  the nested-transaction marker while preserving deadlines, tracing and request
  metadata. Retained sessions and children reject operations after their owning
  callback ends, including error and panic paths.
- A caller can supply an explicit `BeforeCommit(ctx, sess) error` step with its
  transaction request. After a successful body, close its handle and give the step a
  fresh handle on the same transaction, scope, read-only mode and deadline. Close
  that handle before commit. A step error rolls back the attempt. Applications
  compose their steps. Each retry that completes its body successfully runs the step once.

### A2. Context-derived variables

```go
policy := stdsql.ScopePolicy[Scope]{
    ManagedVars: []string{"example.scope", "example.scope_mode", "example.request_id"},
    ScopeVars:   func(s Scope) map[string]string { return map[string]string{"example.scope": s.ID()} },
    ContextVars: func(ctx context.Context) map[string]string { return map[string]string{"example.request_id": requestID(ctx)} },
    IsUnscoped:  func(s Scope) bool { return s.All() },
    UnscopedVars: map[string]string{"example.scope_mode": "all"},
}
```

The application declares the complete set of managed custom variable names at setup.
Validate those names at construction and callback-produced keys at use, before SQL.
Unknown names and collisions with timeout settings are errors. The built-in guard
protects the managed variables on user statements and composes with `GuardQuery`.
Trusted runner setup uses the underlying transaction.

### A3. Scoped and unscoped sessions in one transaction

```go
err := runner.WithinShared(ctx, func(ctx context.Context, tx *stdsql.Shared[Scope]) error {
    all := tx.Unscoped()     // unscoped session, shares the transaction
    one := tx.Scoped(scope)  // scoped session, shares the transaction
    ...
})
```

- One transaction, one role. Before **each** user statement, apply the complete GUC
  state for that handle in one batch. Set the other mode's managed custom variables
  to their declared inactive value (e.g. the empty string agreed with the policy).
  This also clears values left by a previous scope. A failed setup stops the statement.
- Serialize setup **and** execution on the shared transaction, through cursor close
  for `Each` and `Iter`. An overlapping statement or savepoint request returns
  `ErrSessionBusy`. Callers finish a cursor before issuing another statement.
  A savepoint rollback can restore older database settings even when the Go handle
  stays the same. Reapplying every time avoids a cache invalidation protocol. Verify
  scoped → unscoped → scoped, nested savepoint rollback followed by the same handle,
  and child invalidation on both drivers. [PostgreSQL transaction-local settings](https://www.postgresql.org/docs/17/sql-set.html)
- `Unscoped` obeys the runner's `Unscoped` mode from A1.
- **Latency cost**: reapplying the GUC batch before each user statement adds one
  database round trip per statement inside `WithinShared`. This is acceptable for
  background jobs and batch reconciliation, but `WithinShared` is not intended for
  latency-sensitive request hot paths.
- A background job that reads shared configuration unscoped and then writes per-scope
  rows in the same transaction requires `UnscopedReadWrite`. With `UnscopedReadOnly`,
  even reads on the unscoped handle require a read-only shared transaction.

### A4. Sealed sessions

v0.3.0 trusts a session's `Scope()` and `Unscoped()`. In particular, a true
`Unscoped()` makes derived reads omit the row-scope predicate. A registry should be
able to require an instance issued by the application's trusted construction code.
The proof records provenance for in-process code review. The issuer remains
responsible for configuring the session's guards. Privileged reflection/unsafe code
and direct database access lie outside this trust model. Database roles and RLS
enforce access.

```go
// Descriptors can still be defined at package initialization.
var reg = tabula.NewRegistry(tabula.RegistryConfig[Scope]{RequireSealed: true})

// Called once during trusted startup. All runner and Direct factories use this key.
func start(db *sql.DB, policy stdsql.ScopePolicy[Scope]) (*stdsql.Runner[Scope], error) {
    key := tabula.NewSessionKey()
    runner, err := stdsql.NewRunner(db, policy, stdsql.RunnerOptions{SessionKey: key})
    if err != nil {
        return nil, err
    }
    if err := reg.Seal(key); err != nil {
        return nil, err
    }
    return runner, nil
}
```

| Registry construction | Bind accepts | Calling `reg.Seal(key)` |
| --- | --- | --- |
| Default config | v0.3.0 sessions | Error. An open registry can already have usable bindings. |
| `RequireSealed: true`, zero key | No sessions until startup seals it | First nonzero key wins atomically. A zero key or any second call returns an error. |
| Nonzero `SessionKey` in config, or a successfully sealed registry | A proof for this key and this instance | Error, including a repeat with the original key. |

Late sealing lets packages declare descriptors before the application constructs its
runners. Startup must check its own successful seal before exposing the registry or
serving requests. A one-shot setter cannot authenticate its first caller. Keep that
authority in trusted startup code, expose descriptor handles to adapters, and treat an
unexpected earlier seal as a startup failure. Independent tests can supply a key at
construction. Registries never disclose the key.

1. A key identifies a distinct, privately allocated nonzero-size token. Once the
   driver has allocated the final session pointer, it issues an opaque `SessionProof`
   via `key.Sign(owner any) SessionProof` (zero `SessionKey` returns zero
   `SessionProof`), keeping "Seal" strictly for registry sealing (`RequireSealed`,
   `reg.Seal(key)`). The proof stores that token and a strong reference to the
   concrete owner and is exposed via the optional interface
   `interface{ SessionProof() tabula.SessionProof }`. The driver returns its
   original pointer, preserving `Savepoint`, `ReadOnly`, observability and pgx
   `CopyFrom`. (This decision covers proof issuance and the optional accessor only;
   how `RunnerOptions.SessionKey` is wired stays with A1's constructor signature.)
2. Keyed binding checks a non-nil pointer to a nonzero-size owner **before** any
   optional method call. It reads the proof and compares both token identity and
   the owner (same concrete pointer type and instance). A zero/missing proof, a
   typed nil, a value receiver passed by value, a copied session and a forwarding
   wrapper all fail. This avoids interface comparisons on non-comparable values
   and ambiguous zero-size pointer identity. [Go pointer equality](https://go.dev/ref/spec#Comparison_operators)
   Because `Bind` runs on the hot path (often once per operation), implementation
   must include a `BenchmarkSealedBind` benchmark to confirm reflection overhead
   is negligible (or cache per-type eligibility if measurable).
3. `Table.Bind` and `View.Bind` keep their signatures. On rejection they return a
   permanently failed binding. Every error-returning operation gives
   `ErrForeignSession` directly, before scope, read-only checks, caller filters,
   empty-batch shortcuts, logging, `ErrorPolicy.Classify` or driver I/O.
   Iterators yield one zero row and that error, then stop. `WithLogger` retains the
   failure. Pure helpers cannot consult a rejected session (`Rows.Scope` yields
   a false predicate). Sealing later requires a **new** bind. C2 uses the same check.
4. Both drivers must cover ordinary, read-only, pre-commit and retry handles, A3 scoped and
   unscoped handles, and every pointer returned by `DirectPool.Session` (key configured
   at pool construction). A pgx savepoint creates a child pointer and needs a fresh
   proof. A stdsql savepoint reuses its session. A decorator or a test double needs
   a fresh proof from its own trusted factory. Keep `NewSession(tx, scope, policy)`
   as the unsealed migration path. Keyed registries require a keyed construction
   path in **both** drivers.
   Native pgx pool options and read-only startup settings still apply to keyed Direct
   pools. Applications give unit tests an isolated registry, key and pointer-backed
   fake. The pgx module uses only public Tabula APIs.

The production tests in [seal_capability_test.go](../seal_capability_test.go)
exercise the check using `key.Sign` and `SessionProof`, and
`BenchmarkSealedBind` verifies hot-path binding cost. Inventory reports
construction, signing and sealing sites for review (§D).

### A5. No raw transaction escape

Both `stdsql.Session.Tx()` and `pgxdriver.Session.Tx()` expose the underlying
transaction. Move raw access to an explicit driver-specific `RawSession` path
enabled by `RunnerOptions{ExposeTx: true}`. Its creation and use must be visible
in code review and in `tabula inventory`.

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
- Default `Deny` is zero to keep v0.3.0 behaviour.
- `DenyUnscopedWrites` is a runtime check on every derived write, hand-written
  `Rows.Exec` and binary COPY. Provide a public bound-row check (proposed
  `CheckCopy() error`) so `pgxdriver.CopyFrom` checks provenance, mode, `DenyCopy`
  and unscoped-write policy before its empty-input shortcut or driver access.
  It then uses `CheckWrite` on each row to check the row's own scope.

### A7. Transaction-level retry

```go
err := runner.WithinRetry(ctx, scope, tabula.RetryConfig{
    MaxAttempts: 3,
    BaseDelay:   10 * time.Millisecond,
    MaxDelay:    200 * time.Millisecond,
    ShouldRetry: tabula.IsRetryable,
}, fn)
```

- Both drivers call the root `tabula.Retry` with the existing `RetryConfig`, including
  its `Sleep` override. Each attempt opens a new transaction, repeats setup and the
  whole callback, and invalidates its session. Rollback completes before retry.
  Caller cancellation ends the retry loop. Ambiguous commit outcomes require
  application reconciliation.
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

v0.3.0 attaches `Tracer`, `Metrics` and the logger to `RegistryConfig`, so only descriptor
operations are observed. v0.3.1 adds the same interfaces to `RunnerOptions`; every
`Exec`, `Scan`, `Returning` and `Each` on a runner's session is observed, including
hand-written SQL.

- Span and metric names: the registered statement key when known; otherwise
  `tabula.inline` with a stable fingerprint of the normalized SQL text, never argument
  values.
- Attributes include `db.system=postgresql`, role, action, rows,
  the v0.3.0 error class (including `rls_denied` and `insufficient_privilege`), and the
  application's `ScopeLog` label when configured.
- Before calling the session, the descriptor attaches statement metadata to the
  operation context under a private root-package key. Carry the registered key, table
  or view, action, argument secrecy mask, statement-wide `Secret()` and `Quiet()`.
  Each driver consumes this through a public root-package observer or accessor.
  This also works across the pgx module boundary. Inline SQL uses the fingerprint
  only when descriptor metadata is absent.
- Select one owner per enabled signal (trace, metrics, log). The runner takes
  precedence for signals it handles and the registry supplies the others. Keep
  per-binding logger overrides and apply redaction before emitting values. Acceptance
  tests count emissions and inspect redaction for registered and inline SQL, errors,
  early cursor close, and a registry with a custom session.

### B2. OpenTelemetry module

A separate Go module `github.com/0xJean/tabula/oteltabula` (`package oteltabula`)
implements `Tracer` and `Metrics` over the OpenTelemetry API, avoiding import
collisions with `go.opentelemetry.io/otel` and matching `otelsql` / `otelhttp` and
`stdsql` / `pgxdriver`. Applications register this dependency explicitly. Core
packages keep their standard-library dependency boundary.

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
where := func(f *tabula.Filter) { f.And("archived = ?", false) }
peak, present, err := tabula.Max[int64](ctx, rows, "rank", where)
counts, err := tabula.CountBy[string](ctx, rows, "category", where)
ok, err := rows.Exists(ctx, where)
```

- Columns must be declared on the table and, for `CountBy`, whitelisted on the spec
  (`TableSpec.GroupBy`). Scope and soft delete apply as for `Find`.
- `Max[V, T, K, S any](ctx context.Context, rows *Rows[T, K, S], col string,
  where func(*Filter)) (V, bool, error)` is a package function. `Min`, `Sum` and
  `Avg` follow the same contract. Choose V for PostgreSQL's **result** type and
  reuse the registry codec for it. SQL NULL (empty set or all inputs NULL) returns
  the zero V and `false, nil`. A genuine zero returns `true`.
- `CountBy[V comparable]` returns `(map[V]int64, error)` for declared non-null
  grouping keys. An empty input returns an empty map. `Exists(ctx, where)` takes
  the filter callback. The v0.3.0 `ExistsBy(ctx, predicate, values...)` takes one
  static predicate. Record the per-column aggregate SQL in the table definition.
- Custom aggregate result codecs do not make a type eligible for `Add`. In v0.3
  `SetExpr` arithmetic accepts Go integer and floating-point types. Applications
  can register typed, conditional arithmetic as a hand-written `Table.Statement`.

### C2. Composed view queries

A registered `*View[T,S]` owns `view.DefineQuery(key, spec, opts...)`. The plan uses
that view's column order, nested prefixes, custom columns, codecs and explicit grants.
Its verification SQL goes into `ViewDefinition.Statements`. `query.Bind(session)`
inherits the registry's A4 check. Authors supply the SELECT list and all joins.

**Example of emitted PostgreSQL.** This illustrates what the driver receives after
compilation. The agreed authoring shape (`Select / Where / Tail` with fragment-local
`$1…$m`) is pending validation in the v2 fragment-compiler prototype before C2's
public API is locked (see [Interface decisions](#interface-decisions)).

```sql
SELECT n.id, n.rank, n.due_at <= $1 AS due, coalesce(l.caption, '') AS caption
FROM notes n LEFT JOIN labels l ON l.id = n.label_id AND $2::boolean
WHERE (n.visible OR n.pinned)
  AND (n.scope_id = $3)
  AND (n.title ILIKE $4 OR n.code ILIKE $4)
ORDER BY n.rank DESC, n.id DESC LIMIT $5 OFFSET $6
```

**Scope and parameters.** A column-scoped plan gets its scope from the bound session
using the same `RegistryConfig.ScopeID` conversion as `Rows`. A validated unscoped
session omits only that predicate. Request parameters cannot supply or disable it.
Apply the same decision to count, list, find, cursor and streaming reads. An
RLS-dependent query needs an appropriate database role and policies. The example
uses an explicit scope column. The agreed direction is a three-state explicit scope
declaration failing closed on scoped registries (`ScopeColumns`, `ScopeByRLS`,
`ScopeGlobal`), pending v2 prototype validation (see
[Interface decisions](#interface-decisions)).

**Outer-join scope boundary.** Because scope-column predicates are emitted in the
outer `WHERE` clause, they cover only the driving table and inner-joined tables.
Placing a `LEFT JOIN` right-hand table's column in `WHERE` would silently convert
the outer join into an inner join (and break an optimized `CountBase` that omits
that join). For outer-joined tables, correlate the scope in the `ON` clause
(`ON child.scope_id = parent.scope_id`, or via composite foreign key) or rely on
PostgreSQL RLS. Session-bound scope placeholders inside `Select` or `ON` are out
of scope for v0.3.1.

Supply base arguments separately from named filters. Base values can occur in a
SELECT expression, a JOIN condition, a CTE or a subquery. Check base arity as well
as each enabled filter's arity before issuing even a count. A zero-argument filter
is switched on by its presence. A range consumes two values. An array supplied to
`ANY` is one driver-compatible argument. A supplied nil (including a typed nil)
binds SQL NULL. Only omission disables a filter. Unknown or repeated filter names
return an error. Compile once and bind in a canonical order so the same selection
produces identical SQL and argument positions regardless of caller order. Filter
specs should be extensible to carry per-filter metadata such as `Secret` for B1
argument redaction (for example PII email/phone search filters).

**SQL boundaries.** Fixed conditions, the scope and each filter must each constrain
the entire result. Emit parenthesized predicates with newline boundaries; when
`Where`, the scope and every filter are all absent, omit the `WHERE` clause
entirely. Under the agreed `Select / Where / Tail` split, definition-time checks are
lexical only: reject unbalanced or negative parenthesis depth, unterminated quoting
or comments, statement separators, trailing line comments that could consume
generated SQL, and non-contiguous placeholder slots. Tokens inside quoted
identifiers, ordinary/escape strings and dollar-quoted bodies retain their original
bytes. PostgreSQL JSONB operators need round-trip cases. The current lexer discards
comments and token offsets; compilation needs source boundaries and explicit lexical
errors. Because definition-time checks do not parse outer SQL clauses, misplaced
clauses (such as `WHERE` inside `Select` or `ORDER BY` inside `Tail`) surface as
PostgreSQL syntax errors in the C3 `PREPARE` variants, making C3 verification
mandatory for composed queries. A raw view statement remains available for shapes
outside the segmented grammar. Declaration mistakes fail at startup. Errors in
request values are returned to the caller.

| Bound operation | Result and validation |
| --- | --- |
| `List` | Items, total, error. Positive size up to the spec's `MaxLimit`, nonnegative offset. `CountExact` or `CountNone` (total −1). |
| `Find` / `Iter` | Rows or `iter.Seq2[T, error]` with the same predicates and ordering. Neither runs a count. Streaming closes its cursor on break or error. |
| `Count` | Exact number of matching result rows before pagination. |
| `ListKeyset` | Fetch limit + 1, return at most limit items and whether another page exists. The next tuple comes from the **last returned** item when there is another page. |

A fixed keyset order governs both sorting and cursor comparison for every valid
request direction. For a caller-selectable order, the empty direction uses the
declared default. Unknown sort keys, invalid directions,
sizes, counts, unsupported locks and malformed cursors return errors before I/O.
Paged queries must declare a valid `MaxLimit` at definition time (`0 < MaxLimit < math.MaxInt`)
so requests fail closed against unbounded limits and `limit + 1` never overflows;
unbounded reads use `Find` or `Iter`.
A keyset order needs a non-null projected value for each comparison term, its SQL
expression, the matching view column and a unique tie-breaker **on the joined result**.
Reject nullable terms including nullable sides of outer joins, or project an explicit
non-null SQL expression and use that same value for ordering, comparison and extraction.
`After []any` carries typed Go values in that order. Check arity, types and NULLs.
An empty cursor starts the first page. Encoding a cursor for transport belongs to the
application. A single tuple inequality is valid for uniform directions. Mixed directions
require either rejection or an expanded comparison per term (direction agreed for v0.3.1:
reject mixed directions in keyset mode at definition time; lifting this later is
backward-compatible). [PostgreSQL row comparisons](https://www.postgresql.org/docs/17/functions-comparisons.html#ROW-WISE-COMPARISON)

**Exact counts.** Wrap the complete filtered, unpaged query in
`SELECT count(*) FROM (...) AS tabula_count`. This counts groups and DISTINCT rows
correctly. An optional optimized `CountBase` can omit a display-only join when doing
so preserves cardinality; disallow combining `CountBase` with `Tail` (`GROUP BY` /
`HAVING`), which must use the default wrapper. Apply the same fixed, scope and
enabled filter predicates. Each statement has its own dense argument vector, including
only base values it uses: when `CountBase` + `Where` reference only a subset of base
slots (for example `$2` without `$1`), compile-time slot remapping renumbers the
referenced subset to dense `$1…$m` and slices runtime base arguments accordingly
(rejecting out-of-range slots), so PostgreSQL `PREPARE` never sees skipped slots.
Verify every filter against the count source. A missing alias is a verification failure.
The author must demonstrate equal totals on representative data, including join
multiplicity. Use a caller-owned repeatable-read transaction when page and count
must reflect one snapshot. A single table's `CountEstimate` is invalid here.

### C3. Verifying composed queries

Record stable, unique keys on the owning view for the zero-filter baseline, all filters,
each individual filter, each selectable order, first/after cursor shapes and the
default/optimized count paths. Include applicable scoped and unscoped shapes and
exercise the count source with each filter and all filters. Generate in stable order.
`DefinedViews`, snapshots and `VerifyViewStatements` can consume the existing format.
Run PREPARE/EXPLAIN with each intended role. These bounded variants check symbols and
grants. Fixtures separately check scope isolation, equivalent totals, tied sort values,
cursor continuity and streaming early exit.

### C4. Embedded SQL files

```go
//go:embed sql/*.sql
var files embed.FS
var sqlFiles = tabula.MustSQLFiles(files, "sql")
view.Statement("orders.report", sqlFiles.Get("orders_report.sql"))
```

Long joins live in `.sql` files, are loaded once at init and are registered like any
other statement. Missing files fail at start-up. With C2's agreed
`Select / Where / Tail` split, an embedded `.sql` file holds the `Select` segment
(without an outer `WHERE`), while full hand-written statements continue to use
`view.Statement`.

## D. Verification support

- `tabula inventory` recognizes `DefineQuery` and aggregate calls as static sites.
- The workload format accepts composed-query variants from C3 without manual listing.
- Extend inventory's typed call-site collection to include `NewSessionKey`, registry
  sealing, `SessionKey.Sign`, keyed factories, `WithoutTransaction` escape
  sites and both drivers' raw transaction access. Report package, file and line,
  including import aliases and escaped function or method values for review. The v0.3
  SQL-method matcher alone cannot identify package-level functions. Applications own
  any allowed-site rules.

## Interface decisions

The table below records each interface choice, its final status, the chosen
direction and the rejected alternatives (kept for future reference). All seven
decisions are implemented and locked for v0.3.1.

| Area | Status | Chosen direction & rationale | Rejected alternatives & why |
| --- | --- | --- | --- |
| A4 proof issuance | **Decided** | **`key.Sign(owner any) SessionProof`** with optional `interface{ SessionProof() tabula.SessionProof }`. Keeps "Seal" exclusively for registry sealing (`RequireSealed`, `reg.Seal(key)`); needs no scope type parameter on the issuer; zero `SessionKey` returns zero `SessionProof`; simple method-receiver matching in `tabula inventory`. Scope: proof issuance and accessor only (`RunnerOptions.SessionKey` remains part of A1's constructor design). | Package `Seal(session, key)` (overloads "Seal" with `reg.Seal`/`RequireSealed` and reads like an in-place mutator or wrapper); a wrapper returning `Session[S]` (erases concrete driver types such as `*stdsql.Session` / `*pgxdriver.Session` and `Savepoint`/`CopyFrom`). |
| B2 module naming | **Decided** | **`github.com/0xJean/tabula/oteltabula` (`package oteltabula`)**. Avoids forced import aliases alongside `go.opentelemetry.io/otel` and follows both Go OpenTelemetry (`otelhttp`, `otelsql`) and Tabula (`stdsql`, `pqdriver`, `pgxdriver`) naming conventions. | `/otel` import path (`package otel`, collides with `go.opentelemetry.io/otel` in every wiring file). |
| C2 SQL declaration | **Decided** | **`Select / Where / Tail` + optional `CountBase`** (a second `Select` segment sharing `Where` and filters; disallow `Tail` + `CountBase`). Eliminates fixed-predicate drift between page and count, prevents outer `OR` precedence bugs by construction, avoids a security-sensitive outer-clause SQL parser, and supports `GROUP BY`/`HAVING` cleanly. Remap sparse base slots used by `CountBase` + `Where` into dense `$1…$m`. Trade-off: C4 `.sql` files store the `Select` segment (full statements keep using `view.Statement`). | Single constrained `Base` (requires building an outer-clause SQL parser/rewriter, risks outer `OR` precedence errors, and forces duplicating fixed `WHERE` predicates into `CountBase`). |
| C2 filter markers | **Decided** | **Fragment-local `$1…$m`**. Supports reusing one value across many columns (multi-column `ILIKE` search) without duplicating caller arguments, keeps PostgreSQL JSONB `?`/`?|`/`?&` unescaped, and stays uniform with `$1…$k` in `Select`/`Where`. Validate contiguous `1…m` slots at definition (ignoring strings, quoted identifiers, comments and dollar-quoted bodies), and document explicitly that fragment `$1` is local to the fragment, not base `$1`. | Positional `?` with `??` JSONB escaping (cannot reuse one parameter across multiple predicates without duplicating values, collides with PostgreSQL JSONB operators, and mixes `?` with `$n` in `Select`/`Where`). |
| C2 filter API | **Decided** | **Ordered `[]QueryFilter` specs** (`Name`, `SQL`, extensible `Secret bool` for B1 redaction; rendered in **declaration order** so SQL order is visible, stable and rename-safe) **paired with a zero-value-usable `*QueryParams` builder**: `var p tabula.QueryParams` works directly with `p.With(name, args...)` / `p.WithIf(cond, name, args...)` (passed as `&p`), and `NewQueryParams(baseArgs...)` constructs one with base args or for inline chaining. `NewQueryParams` and `With`/`WithIf` defensively copy their argument slices and mutate the receiver in place, returning it. Bound operations (`List`/`Count`/`Find`/`ListKeyset`/`Iter`) accept a `nil` `*QueryParams` as "no base args, no filters"; calling `With`/`WithIf` on a `nil` receiver fails loudly (matching `*Filter`, so imperative `if` calls never silently drop a filter). Because `With` mutates in place, callers can guard pointer dereferences in ordinary `if` blocks (`if since != nil { p.With("since", *since) }`) instead of `WithIf(since != nil, ..., *since)` (whose GoDoc must show that eager-evaluation panic anti-pattern). Once built, one `*QueryParams` is read-only during execution and reusable across operations. | `map[string]string` filter specs (random iteration order requiring alphabetical sort where renaming alters SQL, and no room for per-filter metadata like `Secret`); untyped `map[string]any` or standalone variadic `With` options (ambiguous zero-arg/nil/slice semantics and awkward separation of base args vs filters across `Count`/`List`). |
| C2 scope declaration | **Decided** | **Three-state explicit declaration, fail-closed on scoped registries (`S != struct{}`)**: `ScopeColumns(cols ...string)` (multi-column, sharing one `$n`), `ScopeByRLS()`, or `ScopeGlobal()` (for global lookup-only views). Zero value is a startup error on scoped registries and allowed on `Registry[struct{}]`. Covers driving and inner-joined tables only; outer-joined tables must correlate in `ON`, use a composite FK, or rely on RLS. | Single optional `ScopeColumn` (silent cross-tenant leak when omitted on a scoped registry; cannot cover multiple inner-joined tables or explicitly distinguish RLS vs global lookup views). |
| C2 pagination | **Decided** | **Query-specific request & order types** (`QueryPage`, `QueryKeyset`, `QueryOrder`, `QueryOrderSpec` with `QuerySortTerm{Expr, Column, Direction}` and `SortDirection`) **while reusing `CountMode` (`CountExact`/`CountNone`) and `KeysetPage[T]` with an additive `Next []any` field** (auto-extracted from `View` column accessors and also populated by `Rows.ListKeyset`). Reject mixed directions in keyset mode in v0.3.1 (forward-compatible to expand later), and reject keyset pages on a query that declares `Tail`. Require `0 < MaxLimit < math.MaxInt` on paged queries. | Reusing `Listing` / `KeysetListing` (overloads `Listing.Order` between trusted SQL on `Rows` and untrusted request sort keys on C2, drags in unsupported `Lock` and `CountEstimate`, and lacks cursor column metadata); `Desc bool` (zero value silently overrides `DefaultDirection: "DESC"`). |

### Gaps and implementation constraints identified in review

1. **Outer-join scope limitation (C2)**: `WHERE`-level `ScopeColumns` cannot constrain a `LEFT JOIN` right-hand table without turning it into an `INNER JOIN`, and request inputs cannot supply the scope value to `ON`. v0.3.1 scopes driving and inner-joined tables via `ScopeColumns` and relies on `ON` correlation, composite foreign keys or PostgreSQL RLS for outer-joined tables; session-bound scope placeholders inside `Select`/`ON` are deferred.
2. **C2 fragment compiler & fuzzing**: Implemented in `internal/pgstmt/scan.go`, `composed_compile.go`, `composed_query.go`, and fuzzed via `FuzzCompileQueryFragment` in `fuzz_test.go`, covering fragment lexing, parenthesis/comment/quote/dollar-quote boundaries, local `$1…$m` renumbering and `CountBase` dense base-slot remapping.
3. **A3 per-statement overhead**: `WithinShared` adds one round trip per user statement to reapply the complete GUC set; document it as a background/batch mechanism rather than a hot-path request helper.
4. **A1 nested-transaction escape hatch**: `ErrNested` via context propagation blocks intentional independent transactions (such as failure-safe audit writes); provide `tabula.WithoutTransaction(ctx)` to clear only the nested-tx marker explicitly (and surface call sites in review/inventory).
5. **A4 hot-path `Bind` benchmark**: Prototype validation runs `reflect.ValueOf` on every `Bind`; add `BenchmarkSealedBind` during A4 implementation to confirm acceptable cost or cache per-type eligibility.
6. **Prototype package lifecycle**: `docs/internal/v04` lives inside the root Go module, so it runs with `go test ./...` and would ship in release tags if retained. Remove `docs/internal/v04` once production implementations and tests replace it, before tagging v0.3.1.
7. **Keyset pagination with `Tail` (C2)**: the cursor predicate is emitted in `WHERE`, which runs before `GROUP BY`. An aggregate sort term (for example `count(*)`) cannot be compared there, and a `HAVING` clause can change which rows exist after the cursor. Reject `ListKeyset` on a query that declares `Tail` in v0.3.1, as with `CountBase`. Offset pages, `Find`, `Iter` and the wrapped count remain available.
8. **Checks that remain under `Select / Where / Tail` (C2)**: the segment split removes the need to locate an insertion point, so definition-time checks are lexical only (balanced parentheses, terminated quotes and comments, no statement separator, no trailing line comment, contiguous placeholder slots). A misplaced outer clause, such as a `WHERE` inside `Select` or an `ORDER BY` inside `Tail`, surfaces as a PostgreSQL syntax error in the C3 variants. C3 verification is therefore mandatory for composed queries. Define the emitted text when `Where`, the scope and every filter are absent (no `WHERE` clause).
9. **Nested-transaction marker across modules (A1)**: `ErrNested` must hold between a `stdsql` runner and a `pgxdriver` runner, and the pgx module uses public Tabula APIs only. The marker therefore needs a public root-package setter and query alongside `WithoutTransaction`. Name them with A1 and report their call sites in inventory. GoDoc for `WithoutTransaction` must warn that a detached transaction takes a second pooled connection while the outer one is held, which can deadlock a saturated pool.
10. **Deferred `QueryParams` errors, defensive copies & concurrency (C2)**: a chainable `With` cannot return an error. Unknown or repeated names and arity mismatches are therefore reported by the bound operation, before any I/O. Make the zero value `var p tabula.QueryParams` directly usable (like `strings.Builder` and `Filter`) so request-conditional filters need no nil pointer; calling `With`/`WithIf` on a `nil` `*QueryParams` still fails loudly (never silently drops filters), while bound operations treat a `nil` `*QueryParams` as empty. Both `NewQueryParams` and `With`/`WithIf` copy their input argument slices so later caller mutations cannot affect subsequent `Count`/`List` calls; `WithIf` GoDoc must show the `WithIf(p != nil, ..., *p)` eager-evaluation panic anti-pattern. Like `*Filter`, a `*QueryParams` is not safe for concurrent mutation, but once built it is read-only during execution and safe to reuse across operations (such as `Count` followed by `List`).

## Compatibility

| Change | Impact |
| --- | --- |
| A1 changes both `NewRunner` constructors | Breaking. Supply options and handle the returned error. Review nested calls and use of sessions after callbacks. |
| A2 changes `ScopePolicy` | Breaking. Migrate transaction setup of role/timeouts/GUCs from `SetupTx` to the explicit configuration. Review custom setup and guards. |
| A5 removes both drivers' default `Session.Tx()` | Breaking for raw transaction users. Migrate to the explicit raw path. |
| B1 runner hooks take precedence over registry hooks | Behaviour change only when both are configured |
| A3, A4, A6–A8, C1–C4 | New capabilities. A4 enforcement and A6 denials require explicit configuration. |

[MIGRATION.md](MIGRATION.md) gains a v0.3.0 → v0.3.1 section in the release commit.

## Out of scope

Generated joins, relations in struct tags, preloading, query builders for arbitrary SQL,
role or policy creation, migrations, and any application-specific role names or
variables.

## Delivery order and acceptance

1. Consumers can adopt released v0.3.0 verification and fixture tools independently.
2. Finalize the A1 constructor (`RunnerOptions`, including `SessionKey`, and the
   nested-transaction marker / `WithoutTransaction` escape hatch) alongside the
   decided A4 proof contract (`key.Sign` / `SessionProof`) before integrating keys
   into both drivers. Deliver A1–A8 and the session-related D checks together.
   Validate nested transactions, deliberate detached transactions, read-only
   overrides, savepoint rollback, invalid bindings across all session creation paths
   and `BenchmarkSealedBind` hot-path overhead.
3. Deliver B1–B3 (including the decided `oteltabula` module) for consumers replacing
   their runner and session tracing. Then consumers can narrow grants against the new
   runner and migrate adapters in batches.
4. Validate C2's agreed direction with a second prototype and fuzz seeds covering
   fragment compilation (lexing, parenthesis/comment/quote boundaries, local `$1…$m`
   renumbering and `CountBase` dense base-slot remapping) before locking C2's public
   API and implementing C1–C4 and the query-related D checks. Consumers can then move
   their composed lists onto verified views.
5. Remove the proposal-only `docs/internal/v04` test package once production
   implementations and tests replace it, before tagging v0.3.1.

Run the production unit, fuzz and PostgreSQL integration suites with
`TABULA_TEST_DSN=... ./scripts/check.sh --all`. The production test suites
([seal_capability_test.go](../seal_capability_test.go),
[composed_query_test.go](../composed_query_test.go),
[v031_integration_test.go](../v031_integration_test.go),
[stdsql/v031_integration_test.go](../stdsql/v031_integration_test.go),
[pgxdriver/v031_integration_test.go](../pgxdriver/v031_integration_test.go),
[oteltabula/otel_test.go](../oteltabula/otel_test.go), and
[sqlcheck/login_role_integration_test.go](../sqlcheck/login_role_integration_test.go))
replace the former proposal-only prototype package.

Every item ships with unit tests and real PostgreSQL integration tests (`-tags=integration
-race`, PostgreSQL 14–17), fuzz seeds for any new SQL text handling (C2 fragments, A4
key handling has none), GoDoc, and updates to README, CHANGELOG, COOKBOOK, MIGRATION and
REFERENCE in the same commit, per [AGENTS.md](../AGENTS.md).
