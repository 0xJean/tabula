# Changelog

All notable changes are listed here. The project follows semantic versioning; before
v1.0 a minor version may contain breaking changes.

## v0.4.4 — unreleased

- Establish shared lexical query, result-identity and write-effect contracts for composable statements. The implementation follows [the M3 design](docs/DESIGN-M3.md).
- Add the optional `OperationSession` capability and the standard-library `sqlcheck.Verifier` transaction/description contract. Concrete plan and driver integration is in progress; existing execution behavior is unchanged.

## v0.4.3 — 2026-11-27

- Typed scalar expressions cover conditionals, arithmetic, casts, text, regular expressions, time, JSON, arrays, encoding, hashing and UUID generation. Use them in projections, predicates and conditional updates. Assignment targets require UPDATE. Evaluating a value or predicate requires SELECT on the columns it reads. [Scalar expressions](docs/EXPRESSIONS.md)
- Typed grouping and ordering accept computed expressions and explicit NULL placement. `RowsPlan.CursorFrom` reads projected ordering values, including reordered table selections; `AtExpr` and `NullAtExpr` supply manual expression boundaries.
- Aggregates support `FilterWhere`, `CountDistinct`, `DistinctInputs`, ordered string/array/JSON/JSONB inputs and `BoolOr`/`BoolAnd`. `ArrayAggAs` checks the declared element type and requires a codec preserving NULL elements for nullable inputs. [Aggregate inputs](docs/COOKBOOK.md#computed-groups-and-aggregate-inputs)
- `ReadSelect.Distinct` and `DistinctOn` work with single sources and joins. Representative selection requires a repeatable winner order with the complete result key; invalid grouping, modifiers, types and locking combinations fail before registration.
- Explicit reads add `ReadListSpec`, `WithList`, `Page`, `List`, `Count`, `Cursor` and `CursorFrom`. List and fixed forms share a registration budget; exact counts retain grouped and distinct cardinality with independent minimum grants and bindings. Joined cursors use the complete projected result key and seek after representative selection. Catalog readiness and role snapshots retain immediate primary-key and deterministic comparison premises. [Joined lists](docs/COOKBOOK.md#finite-lists-on-explicit-reads)

- `DefineTableRead` and `TryDefineTableRead` apply SELECT clauses to a mapped table selection and reuse its existing result decoders. [Mapped table reads](docs/COOKBOOK.md#mapped-table-reads)
- `pgtype.ArrayOfNullable` and both driver array-column adapters preserve SQL NULL elements in a one-dimensional array of scalar pointers. A typed binding copies those scalar values. Array columns also accept ordinary `[]int16`, `[]uint32` and `[]float32` for non-NULL elements. [PostgreSQL arrays](docs/COOKBOOK.md#13-postgresql-network-addresses-and-arrays)

## v0.4.2 — 2026-11-27

- `DefinePlanSource` and `TryDefinePlanSource` register a physical table for typed plans. The declared plans determine its SQL and grants, including column-level updates and locking reads on composite keys. Legacy `Rows` calls return `ErrOperationDenied` before database I/O. [Physical plan sources](docs/AUTHORIZATION.md#physical-sources-for-typed-plans)
- `MapTable` reuses physical field declarations and result decoders across query aliases. Set registry-wide Go type defaults with `TypeMapping` and override individual columns with `ColumnType`. `DefineTableRows` returns a full or partial table record. `DefineExists` registers just the boolean query. [Mapped table reads](docs/COOKBOOK.md#mapped-table-reads)
- Single-table and joined reads support `One` and `MaybeOne` for exact cardinality. Opt in to `First` and `MaybeFirst` with an ordered or explicitly unordered bounded shape. Both optional methods distinguish an absent row from a present zero value. [Read forms](docs/MIGRATION.md#from-v041-to-v042)
- The `pgtype` package supplies PostgreSQL types for typed columns and bindings. Built-in slice bindings preserve their contents across executions. `ConstraintName` extracts a structured constraint name from a wrapped driver error. [Type and binding examples](docs/COOKBOOK.md#postgresql-types-and-binding-values)

## v0.4.1 — 2026-11-26

- `DefineRead` registers explicit inner, left and cross joins, including self-joins. A left join keeps right-side scope and live-row filters in its match condition. Each occurrence contributes its column grants and row-security requirements. [Explicit joins](docs/COOKBOOK.md#explicit-joins)
- `In`, `Any`, `Like` and `ILike` compose with typed predicates. An `ArrayOf` input binds a variable-size list to one PostgreSQL parameter. A single slice binding supplies both statements in `RowsPlan.List`. [Migration guide](docs/MIGRATION.md#from-v040-to-v041)

## v0.4.0 — 2026-11-26

- Use `RowsSpec.Lock` to lock filtered, ordered or paginated rows with `plan.Bind(session).Find`. Transaction advisory locks support shared and exclusive acquisition in both PostgreSQL key spaces. Routine and advisory plans use the same session-binding style.
- `RowsSpec.WithExists` registers a boolean `SELECT EXISTS` per scope and filter. `IsDistinct` and `IsNotDistinct` provide null-safe inequality and equality. Role inventory binds existence checks independently of data and count statements.
- Declare scalar and set-returning functions by exact input and result types. Registered calls preserve the declared read or write effect. Role readiness can verify the function signature, catalog properties and effective privileges, including `UPDATE` on any column for a row lock. [Examples](docs/COOKBOOK.md#registered-locks-and-functions)

- Typed relation occurrences, fields, input slots and projections carry their PostgreSQL types through compilation. Each registered shape records its ordered parameters, output columns, effects and grants in `Definition.StatementShapes`.
- The adapter interface accepts a statement issued by its registry for one session and operation. A snapshot can be inspected independently of execution. [Typed query design](docs/DESIGN-v0.4.md)

- A registry can enable `DenyLegacySQL` and issue its `SQLAuthority` to selected runners or Direct pools. They execute registered calls for that registry and reject direct SQL text, undeclared variants and raw transaction entry. Fixed generated table operations use the same credential. [Registered execution](docs/AUTHORIZATION.md#registered-execution)

- Typed input slots now render an explicit cast to their schema-qualified PostgreSQL type. Regenerate statement snapshots before comparing or verifying plans. `Varchar(n)` records a physical column length for catalog checks. Parameter casts omit the length so PostgreSQL handles comparisons and assignments with their normal semantics.

- Named row plans register typed reads, grouped and scalar aggregates, and affected-count updates and deletes. Role-bound SQL snapshots include every declared alternative and its catalog types and minimum grants. [Row plan examples](docs/COOKBOOK.md#registered-single-table-reads-and-writes)

## v0.3.11 — 2026-11-26

- `TableSpec.WriteMode` supports read and update access with an optional `UpdateColumns` list, and strict insert access with optional serial sequence usage. The descriptor derives statements and privileges for the selected operations. Bound methods and pgx COPY check the mode before database I/O.
- `WriteInsertOnly` supports a named, targetless `ON CONFLICT DO NOTHING` plan. Single inserts leave generated serial keys in PostgreSQL. [Table write modes](docs/AUTHORIZATION.md#table-write-modes)
- Local publication checks cover staged changes, commit messages, branch names, pull request text and release notes. [Contribution workflow](CONTRIBUTING.md#publication-policy)

## v0.3.10 — 2026-11-26

- `postgresfixture.Fixture.Prepare(ctx, plan)` installs the cached template once or refreshes its last-use mark. It requires no login lease and creates no test clone. When migrations create member roles, call `Prepare`, acquire the process login with those roles in `LoginRoleOptions.MemberOf`, then call `CloneWithLease`. [Cold-start example](docs/AUTHORIZATION.md#process-login-roles)

## v0.3.9 — 2026-11-26

- `postgresfixture.LoginRoleOptions.Label` adds an optional readable suffix to each independently generated process login. Existing unlabeled roles retain their lease and cleanup behavior.
- `Fixture.CloneWithLease` binds a test database to a process login. `Prune` and the standalone `PruneClones` can reclaim the database after the process exits and all clients disconnect. [Ownership and cleanup](docs/DESIGN-v0.3.9.md)

## v0.3.8 — 2026-11-26

- A disposable `postgresfixture.Fixture` can acquire a distinct login per test process. A dedicated maintenance session protects it while business connections are idle. `Release` removes the login and its database CONNECT grants at shutdown. `Prune` reclaims abandoned logins after the configured idle interval. [Fixture login lifecycle](docs/DESIGN-v0.3.8.md)

## v0.3.7 — 2026-11-25

- By default, statement failures sent to tracers and logs carry the error class and optional SQLSTATE. The opt-in `RawErrorDiagnostics` exposes original errors to custom tracers and driver messages in OpenTelemetry exception events and span status descriptions. Metrics keep bounded labels in either mode. Database calls still return the original error chain. A configured `Sanitize` supplies application-reviewed log text. [Telemetry migration](docs/MIGRATION.md#from-v036-to-v037)
- `sqlcheck.CheckReadiness` checks minimum effective privileges and required RLS flags for a named role in one catalog query. It reports missing objects alongside missing capabilities. [Startup example](docs/AUTHORIZATION.md#check-a-role-at-startup)
- Both drivers support opt-in `RunnerOptions.RequireCallbackContext`. A live session paired with an unrelated context returns `tabula.ErrCallbackContext`. [Callback ownership](docs/DESIGN-v0.3.7.md#callback-ownership)

## v0.3.6 — 2026-11-25

- In both drivers, a failed top-level commit matches `tabula.ErrCommit` and preserves the driver error. This covers ordinary and shared transactions. A PostgreSQL rejection retains its SQLSTATE. A lost reply can leave a successful commit unconfirmed.
- `RunnerOptions.SingleStatement` checks for one PostgreSQL data statement before session I/O. `RunnerOptions.Statements` adds per-method command allowlists and an optional outer `RETURNING` requirement. A configured method policy includes the single-statement check. Both options default to off. [Migration guide](docs/MIGRATION.md#from-v035-to-v036)
- Both Direct adapters continue to require a single read query. Literal quotes whose interpretation depends on the connection's backslash setting now require an explicit escape string or a bind value.

## v0.3.5 — 2026-11-25

### Additions

- `sqlcheck.SQLRoutineMethod` and `InventoryConfig.RoutineMethods` collect static `SELECT [* FROM] <schema>.<name>($1, ...)` calls from helper methods that take a compile-time routine name and a fixed argument-slice parameter. A nil or dense slice literal determines the arity. Runtime slices and variadic database inputs remain dynamic.
- `sqlcheck.PolicyRule.Expression` and `NotExpression` match or exclude Go regular expressions over the combined `USING` and `WITH CHECK` predicates in `ClassifyPolicies` and `tabula check-policies`.
- `sqlcheck.Statement.NoAccess` and `SQLBinding.NoAccess` declare reviewed complex statements (such as `SECURITY DEFINER` set-returning routine calls) that access zero caller-RLS relations in `CheckPolicyCoverage` and `tabula check-coverage`, while rejecting `NoAccess` on statements with inferred table access.

### Fixes

- `sqlcheck.CollectSQL` resolves `+` string concatenations containing single-assignment local string variables as static SQL, including locals used more than once. Reassigning a parameter in a mixed short declaration keeps it dynamic.
- `sqlcheck.CheckPolicyCoverage` recognizes `IS [NOT] DISTINCT FROM` predicates and parenthesized `FROM` arguments (such as `EXTRACT` and `TRIM` in `UPDATE`/`DELETE`) as single-table statements. An expression using only parameters or literals in those positions adds no column-read requirement.

## v0.3.4 — 2026-11-25

### Additions

- `postgresfixture.Fixture.Prune(ctx, maxIdle, keep...)` drops idle templates and abandoned staging databases through the host backend. It checks the fixture's digest naming format and the template's last-use mark against the server clock. It preserves explicitly kept templates, active copies and templates with missing or invalid marks.

### Changes

- A build uses the deterministic `tabula_stg_<digest>` staging database and recovers its abandoned stage on retry. Parallel copies share a template lock until the physical copy finishes. [Fixture migration notes](docs/MIGRATION.md#from-v033-to-v034)

## v0.3.3 — 2026-11-24

### Security

- `GuardNoSessionEscape`, `GuardReadOnlyTransaction` and any configured `NewSessionGuard` with an active restriction now reject `UPDATE pg_settings` (with `ONLY`, `pg_catalog` qualification, comments or inside a CTE). PostgreSQL treats an update to an existing settings row as `SET` on that parameter, subject to the database role’s privileges.

### Additions

- `SessionGuardConfig.ForbidVariableReads` rejects reading protected variable namespaces: `current_setting` with a protected or non-literal key, `SHOW` of a protected name or `SHOW ALL`, and access to the `pg_settings` view. A result column may be named `pg_settings`, including when used as a sort key. Configure at least one entry in `VariablePrefixes` to enable this option.

## v0.3.2 — 2026-11-24

v0.3.2 includes the work from the untagged v0.3.1 candidate. Applications can upgrade directly from v0.3.0. [Migration guide](docs/MIGRATION.md#from-v030-to-v032)

- Transactions and savepoints close unfinished streaming cursors before committing or rolling back. With a live context, a later pull reports `ErrSessionClosed`. A canceled context keeps its cancellation error. Call `stop` to release the Go producer when using `iter.Pull2`.
- `UnscopedReadRole` lets a shared transaction read with a separately provisioned PostgreSQL reader role and write with its scoped role. Both handles see the transaction's own writes. Before committing, the runner restores the scoped role so deferred invoker triggers use it regardless of the last handle.
- `TableSpec.Aggregates` registers selected `Max`, `Min`, `Sum` and `Avg` calls for SQL snapshots and role verification, including scoped and soft-delete predicates.
- `QuerySpec.MaxOffset` sets an optional inclusive offset limit. Invalid `Rows.ListKeyset` listing options now return an error before query execution.

### Public API changes

- **Role-bound runners & explicit policies (A1–A2)**: `stdsql.NewRunner(db, policy, opts)` and `pgxdriver.NewRunner(pool, policy, opts)` take `RunnerOptions` (`Role`, `RequireRole`, `ReadOnly`, `Unscoped`, `UnscopedReadRole`, `Timeouts`, `SessionKey`, `ExposeTx`, `Tracer`, `Metrics`, `Log`, `Sanitize`, `TagStatements`) and return `(*Runner[S], error)`. `ScopePolicy` replaces `SetupTx` with declarative `ManagedVars`, `ScopeVars`, `ContextVars`, `UnscopedVars`, `InactiveVars`, and `ScopeLog`; runners auto-guard managed variables and timeout settings, reject nested runner calls with `ErrNested` (opt-out via `tabula.WithoutTransaction(ctx)`), close handles on callback return (`ErrSessionClosed`), and support `RunOptions.BeforeCommit`.
- **Opt-in raw transaction escape (A5)**: removed default `Session.Tx()` on both `stdsql` and `pgxdriver`; raw access now requires `RunnerOptions{ExposeTx: true}` and `WithinRaw` / `WithinRawOptions` / `WithinRawTxOptions` returning `*RawSession[S]`.
- **Session-level observability precedence (B1)**: when both a runner and a registry configure telemetry, the runner takes precedence per enabled signal (`Tracer`, `Metrics`, `Log`) so descriptor statements are emitted once while hand-written inline SQL is also observed.
- Overlapping statements and savepoint requests on the same transaction return `tabula.ErrSessionBusy` in both drivers. This includes using a second shared handle while a row cursor is open.
- Both drivers establish PostgreSQL read-only mode before the callback, even with an empty scope policy and zero timeouts. Guarded sessions reject `SET TRANSACTION` and changes to the current or default read-only mode with an error matching `tabula.ErrReadOnly` and `tabula.ErrForbiddenStatement` and remain usable for reads.
- `GuardNoSessionEscape` and any configured `NewSessionGuard` protecting role, session authorization, or managed variables now reject transaction-ending commands (`COMMIT`, `END`, `ABORT`, non-savepoint `ROLLBACK`, `PREPARE TRANSACTION`) with `tabula.ErrForbiddenStatement`, preventing read-write guarded sessions from ending the runner transaction early and falling back to the login role. Savepoints remain allowed.
- Without a dedicated `UnscopedReadRole`, `UnscopedReadOnly` requires the entire shared transaction to be read-only for unscoped access. In a read-write `WithinShared`, even `SELECT` on `Shared.Unscoped()` returns `tabula.ErrUnscopedRequiresReadOnly`. It also matches `tabula.ErrReadOnly` through `errors.Is`.
- Composed keyset queries compare `After` in the declared sort direction when a sort is fixed, including when the request specifies the opposite direction.
- Composed queries reject parameter placeholders in sort and tie-breaker expressions at definition time. Quoted text and comments can contain literal `$n` text.

### Additions

- **Scoped and unscoped sessions in one transaction (A3)**: `Runner.WithinShared` / `WithinSharedOptions` and `Shared[S]` (`Scoped`, `Unscoped`) reapply the complete managed GUC batch before every statement and serialize cursor reads.
- **Sealed sessions & registry capability policy (A4, A6)**: `NewSessionKey`, `(SessionKey).Sign(owner)`, `SessionProof`, `RegistryConfig.SessionKey` / `RequireSealed`, `Registry.Seal(key)`, `ErrForeignSession`, keyed `stdsql.DirectSessionKey(key)` and `pgxdriver.DirectWithConfig`, `RegistryConfig.Deny` (`DenyDelete`, `DenyUpsert`, `DenyUnscopedWrites`, `DenyCopy`), `ErrCapabilityDenied`, and `Rows.CheckCopy()` enforced before empty-input shortcuts in `pgxdriver.CopyFrom`.
- **Transaction-level retry & login-role hygiene (A7, A8)**: `Runner.WithinRetry` / `WithinRetryWith` on both drivers; `sqlcheck.CheckLoginRole` (`LoginRoleExpectation`) and `tabula check-login-role` CLI auditing `LOGIN NOINHERIT`, exact role memberships (including PostgreSQL 16+ per-membership `pg_auth_members.inherit_option`), direct table/column privileges, object ownership, and schema `CREATE` privileges.
- **OpenTelemetry module & server-side slow log correlation (B1–B3)**: new separate Go module `github.com/0xJean/tabula/oteltabula` (`NewTracer`, `NewMetrics` with bounded-cardinality metric attributes omitting `tabula.scope` and `tabula.key`); `tabula.Observer`, `StatementMeta`, `SQLFingerprint`, `SQLAction`, `RunnerOptions.TagStatements` (`/* tabula:<key> */`), and server-side logging guidance in [docs/LOGGING.md](docs/LOGGING.md).
- **Single-table aggregates, composed view queries & embedded SQL files (C1–C4)**: `tabula.Max`, `Min`, `Sum`, `Avg`, `CountBy` (`TableSpec.GroupBy`), `Rows.Exists(ctx, where)`; `View.DefineQuery` / `TryDefineQuery` (`QuerySpec` with `Select`/`Where`/`Tail`/`CountBase`, fragment-local `$1…$m`, `ScopeColumns`/`ScopeByRLS`/`ScopeGlobal`, ordered `QueryFilter` with `Secret`, zero-value `QueryParams`, `List`/`Count`/`Find`/`Iter`/`ListKeyset` with `KeysetPage.Next`, C3 deterministic variant registration on `ViewDefinition.Statements`, and `FuzzCompileQueryFragment`); `LoadSQLFiles` / `MustSQLFiles` / `SQLFiles`.
- **Extended source inventory & workload binding (D)**: `sqlcheck.CollectSQL` recognizes `View.DefineQuery` and single-table aggregates as static sites, collects typed security/factory/raw-tx call sites (`CallTarget`, `CallSite`, `DefaultSecurityCallTargets`, `InventoryConfig.AuditSecurityCalls`), and expands composed-query C3 variants automatically via `RoleStatements.AddViews`, `SQLInventory.BindRolesWithDefinitions`, and `tabula inventory --snapshot`.

Role bindings expand only statements registered for the selected descriptor and query. Snapshots now record `Definition.StatementQueries` in header comments. Regenerate earlier snapshots before using automatic composed-query binding. An unresolved owner or missing aggregate fails the inventory check with the site ID. Manual variants carry their own access requirements. Typed field inventory includes promoted fields. [Authorization guide](docs/AUTHORIZATION.md)

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
