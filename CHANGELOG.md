# Changelog

All notable changes are listed here. The project follows semantic versioning; before
v1.0 a minor version may contain breaking changes.

## v0.3.1 — 2026-11-24

### Public API changes

- **Role-bound runners & explicit policies (A1–A2)**: `stdsql.NewRunner(db, policy, opts)` and `pgxdriver.NewRunner(pool, policy, opts)` take `RunnerOptions` (`Role`, `RequireRole`, `ReadOnly`, `Unscoped`, `Timeouts`, `SessionKey`, `ExposeTx`, `Tracer`, `Metrics`, `Log`, `Sanitize`, `TagStatements`) and return `(*Runner[S], error)`. `ScopePolicy` replaces `SetupTx` with declarative `ManagedVars`, `ScopeVars`, `ContextVars`, `UnscopedVars`, `InactiveVars`, and `ScopeLog`; runners auto-guard managed variables and timeout settings, reject nested runner calls with `ErrNested` (opt-out via `tabula.WithoutTransaction(ctx)`), close handles on callback return (`ErrSessionClosed`), and support `RunOptions.BeforeCommit`.
- **Opt-in raw transaction escape (A5)**: removed default `Session.Tx()` on both `stdsql` and `pgxdriver`; raw access now requires `RunnerOptions{ExposeTx: true}` and `WithinRaw` / `WithinRawOptions` / `WithinRawTxOptions` returning `*RawSession[S]`.
- **Session-level observability precedence (B1)**: when both a runner and a registry configure telemetry, the runner takes precedence per enabled signal (`Tracer`, `Metrics`, `Log`) so descriptor statements are emitted once while hand-written inline SQL is also observed.
- Overlapping statements and savepoint requests on the same transaction return `tabula.ErrSessionBusy` in both drivers. This includes using a second shared handle while a row cursor is open.
- Both drivers establish PostgreSQL read-only mode before the callback, even with an empty scope policy and zero timeouts. Guarded sessions reject `SET TRANSACTION` and changes to the current or default read-only mode with an error matching `tabula.ErrReadOnly` and `tabula.ErrForbiddenStatement` and remain usable for reads.
- `GuardNoSessionEscape` and any configured `NewSessionGuard` protecting role, session authorization, or managed variables now reject transaction-ending commands (`COMMIT`, `END`, `ABORT`, non-savepoint `ROLLBACK`, `PREPARE TRANSACTION`) with `tabula.ErrForbiddenStatement`, preventing read-write guarded sessions from ending the runner transaction early and falling back to the login role. Savepoints remain allowed.
- `UnscopedReadOnly` requires the entire shared transaction to be read-only. In a read-write `WithinShared`, even `SELECT` on `Shared.Unscoped()` returns `tabula.ErrUnscopedRequiresReadOnly`. It also matches `tabula.ErrReadOnly` through `errors.Is`.
- Composed keyset queries compare `After` in the declared sort direction when a sort is fixed, including when the request specifies the opposite direction.
- Composed queries reject parameter placeholders in sort and tie-breaker expressions at definition time. Quoted text and comments can contain literal `$n` text.
- See [MIGRATION.md](docs/MIGRATION.md#from-v030-to-v031) for upgrade details.

### Additions

- **Scoped and unscoped sessions in one transaction (A3)**: `Runner.WithinShared` / `WithinSharedOptions` and `Shared[S]` (`Scoped`, `Unscoped`) reapply the complete managed GUC batch before every statement and serialize cursor reads.
- **Sealed sessions & registry capability policy (A4, A6)**: `NewSessionKey`, `(SessionKey).Sign(owner)`, `SessionProof`, `RegistryConfig.SessionKey` / `RequireSealed`, `Registry.Seal(key)`, `ErrForeignSession`, keyed `stdsql.DirectSessionKey(key)` and `pgxdriver.DirectWithConfig`, `RegistryConfig.Deny` (`DenyDelete`, `DenyUpsert`, `DenyUnscopedWrites`, `DenyCopy`), `ErrCapabilityDenied`, and `Rows.CheckCopy()` enforced before empty-input shortcuts in `pgxdriver.CopyFrom`.
- **Transaction-level retry & login-role hygiene (A7, A8)**: `Runner.WithinRetry` / `WithinRetryWith` on both drivers; `sqlcheck.CheckLoginRole` (`LoginRoleExpectation`) and `tabula check-login-role` CLI auditing `LOGIN NOINHERIT`, exact role memberships (including PostgreSQL 16+ per-membership `pg_auth_members.inherit_option`), direct table/column privileges, object ownership, and schema `CREATE` privileges.
- **OpenTelemetry module & server-side slow log correlation (B1–B3)**: new separate Go module `github.com/0xJean/tabula/oteltabula` (`NewTracer`, `NewMetrics` with bounded-cardinality metric attributes omitting `tabula.scope` and `tabula.key`); `tabula.Observer`, `StatementMeta`, `SQLFingerprint`, `SQLAction`, `RunnerOptions.TagStatements` (`/* tabula:<key> */`), and server-side logging guidance in [docs/LOGGING.md](docs/LOGGING.md).
- **Single-table aggregates, composed view queries & embedded SQL files (C1–C4)**: `tabula.Max`, `Min`, `Sum`, `Avg`, `CountBy` (`TableSpec.GroupBy`), `Rows.Exists(ctx, where)`; `View.DefineQuery` / `TryDefineQuery` (`QuerySpec` with `Select`/`Where`/`Tail`/`CountBase`, fragment-local `$1…$m`, `ScopeColumns`/`ScopeByRLS`/`ScopeGlobal`, ordered `QueryFilter` with `Secret`, zero-value `QueryParams`, `List`/`Count`/`Find`/`Iter`/`ListKeyset` with `KeysetPage.Next`, C3 deterministic variant registration on `ViewDefinition.Statements`, and `FuzzCompileQueryFragment`); `LoadSQLFiles` / `MustSQLFiles` / `SQLFiles`.
- **Extended source inventory & workload binding (D)**: `sqlcheck.CollectSQL` recognizes `View.DefineQuery` and single-table aggregates as static sites, collects typed security/factory/raw-tx call sites (`CallTarget`, `CallSite`, `DefaultSecurityCallTargets`, `InventoryConfig.AuditSecurityCalls`), and expands composed-query C3 variants automatically via `RoleStatements.AddViews`, `SQLInventory.BindRolesWithDefinitions`, and `tabula inventory --snapshot`.

## v0.3.0 — 2026-11-24

### Public API changes

- Declare tables with `TableSpec[T, K]` and projections with `ViewSpec[T]`. The registry supplies the scope type. Compiled handles have read-only schema and access modes.
- Session, Rows and ViewRows execution methods use the bound scope (`Session.Scope()`, `Session.Unscoped()`, `db:"...,scope"`, `RegistryConfig.ScopeID`/`ScopeWrap`, `ScopePolicy.IsUnscoped`). Generated row writes validate row scope identity, including batch upserts and pgx COPY.
- Use the root package for Retry and SQLSTATE predicates. Use LockMode values for Listing and KeysetListing.
- See [MIGRATION.md](docs/MIGRATION.md) for the old and new call sites.

### Additions

- A [v0.3 integration guide](docs/INTEGRATION.md) maps application tasks to the complete [Go API and CLI reference](docs/REFERENCE.md). CI checks reference drift and runs the authorization guide's JSON examples. Release verification checks the guide's version series and the candidate reference. `tabula --help` lists individual commands. Every subcommand's `-h` prints its flags and exits successfully without displaying the configured DSN.

- PostgreSQL authorization auditing covers typed Go source inventory, per-role workloads, exact direct grant manifests, transactional privilege trials, four catalog snapshots, policy classification, command coverage and rollback-based RLS behavior matrices. The CLI accepts application-owned JSON configurations. [Authorization guide](docs/AUTHORIZATION.md)
- Added `tabula.NewSessionGuard` and separate SQLSTATE 42501 classes and sentinels for RLS denials and insufficient privileges.
- The built-in and configurable session guards share one implementation. The built-in guards reject `DISCARD ALL` and Unicode-escaped identifiers. Any active session restriction rejects anonymous `DO` blocks. GUC prefix matching recognizes names beginning with `session.` or `local.`, including after `SET LOCAL` and `SET SESSION`.
- Policy coverage recognizes all four top-level row-locking clauses and handles `FOR` inside expressions such as `substring`.
- The standard-library `postgresfixture` package caches installation templates and clones an isolated database for each test.
- `PrepareStatement` uses a private prepared statement and releases its savepoint. Application-owned prepared statements survive both successful and failed verification.
- **Conditional, upsert, delete, expression & batch writes**: `Rows.UpdateWhere`,
  `Rows.SetWhere`, `Table.DefineUpsert` / `TryDefineUpsert` + `Rows.Upsert` / `UpsertMany`
  (forbids updating key, scope, and `once` columns; enforces scope on conflict; registers
  SQL for `sqlcheck`), `Rows.SetExpr` (`Add`, `SetNow`), opt-in physical deletes
  (`TableSpec.AllowDelete` + `Rows.Delete` / `Rows.DeleteWhere`), `TableSpec.ReadOnly` mode
  (`ErrReadOnly`), `Rows.InsertReturning`, `Rows.SaveReturning`, and chunked multi-row
  `Rows.InsertMany`.
- **Streaming, locking, queries & views**: `iter.Seq2` iterators (`Rows.Iter`,
  `Rows.SelectIter`, `ViewRows.Iter`), `LockMode` (`LockUpdate`, `LockSkipLocked`,
  `LockNoWait`, `LockShare`), `Rows.ListKeyset` cursor pagination, `Listing.Count`
  (`CountExact`, `CountNone`, `CountEstimate`), `Filter.AndIf` / `In` / `Range`, and
  `View` nested struct prefixes with `View.SelectAs`.
- **Schema shapes & codecs**: composite keys (comparable struct `K` with tuple `IN` for
  `GetMany` / `LockMany`); configurable soft delete (`*time.Time`, `bool`, or status value
  via `TableSpec.SoftDelete`); built-in `net/netip.Addr` and `netip.Prefix` codecs;
  `int32`, `float64`, and `bool` array codecs alongside `string` and `int64` in both drivers.
- **Drivers, sessions & transactions**:
  - `stdsql` (stdlib only): `Runner`, `Session`, `ScopePolicy` (RLS `SetLocalConfig` /
    `SetLocalRole` + `GuardQuery`), `Savepoint`, read-only `Direct` pools (with pool sizing
    and `Stats()`), and consistent `ErrReadOnly` enforcement on `WithinReadOnly`.
  - `pgxdriver` (separate Go module): `pgx/v5` pool `Runner`, `pgx.Tx` `Session`,
    `ScopePolicy`, `Savepoint`, `Direct` pools (`DirectOption`, `Stat()`), `CopyFrom`
    (`COPY` wire-protocol bulk insert), and native array converters.
- **Security & RLS tooling**: `GuardNoSessionEscape` (`ErrForbiddenStatement`) and
  `GuardReadOnly` (with PostgreSQL comment, dollar-quote, and `E'...'` escape-string
  handling); `Table.RequiresFunction` and `View.Requires` / `RequiresFunction` for `DELETE`
  and function `EXECUTE` privilege checks; `sqlcheck.CheckRLS` and `tabula check-rls` CLI.
- **Views & materialized views in `sqlcheck`**: `ViewsToDefinitions`, `RenderAllSnapshot`,
  `VerifyViewStatements`, `VerifyViewSchema` (supports PostgreSQL regular views and
  materialized views via `pg_attribute` fallback), `sqlcheck.ParseSnapshot`, and `cmd/tabula`
  CLI (`tabula check` & `tabula check-rls`).
- **Errors & observability**: `SQLState`, `ClassifyError`, `ErrorClass`, sentinels
  (`ErrUnique`, `ErrForeignKey`, `ErrCheck`, `ErrNotNull`, `ErrSerialization`,
  `ErrDeadlock`, `ErrCanceled`), `ErrorPolicy.Sanitize`, opt-in `log/slog`
  (`LogConfig`, `LogLevelVar`, `Registry.SetLogLevel`, `ScopeLog`, `WithLogger`,
  `db:"...,secret"`, `Quiet()`, `Secret()`), and `Tracer` / `Metrics` hooks.
- **Performance, quality gates & ecosystem**: static struct-offset field accessors and
  reused scan buffers; 5 fuzz targets; microbenchmarks, real-PG benchmarks, and the
  cross-library `benchmarks/` comparison module; runnable `examples/multitenant` RLS demo;
  Go 1.26/1.27 × PostgreSQL 14–17 CI with `.golangci.yml`, `scripts/check.sh`, coverage
  gates, `govulncheck`, release tooling (`scripts/release.sh`), `AGENTS.md`,
  `docs/TESTING.md`, `docs/COOKBOOK.md`, `CONTRIBUTING.md`, and `SECURITY.md`.

## v0.2.0 — 2026-11-24

First public release, extracted from a production refactoring project.

- `Table[T, K, S]` and `Rows`: get, lock, insert, save, set, soft delete, count, list,
  find, exists, `GetMany`, `LockMany` (key order), `LockBy`.
- `View[T, S]`: decode rows of hand-written SQL by `db` tags.
- `Filter`, `OrderSpec`, JSON / zero / array codecs, privilege derivation.
- `pqdriver`: lib/pq SQLSTATE helpers and array converters.
- `sqlcheck`: SQL snapshots, schema and role-grant checks, `PrepareStatement` and
  `VerifyStatements` (PREPARE plus EXPLAIN EXECUTE).
