# Changelog

All notable changes are listed here. The project follows semantic versioning; before
v1.0 a minor version may contain breaking changes.

## Unreleased — v0.3 development

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
