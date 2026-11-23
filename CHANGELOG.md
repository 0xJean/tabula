# Changelog

All notable changes are listed here. The project follows semantic versioning; before
v1.0 a minor version may contain breaking changes.

## Unreleased — v0.3 development

### Public API changes

- Declare tables with `TableSpec[T, K]` and projections with `ViewSpec[T]`. The registry supplies the scope type. Compiled handles have read-only schema and access modes.
- Session, Rows and ViewRows execution methods use the bound scope. Generated row writes validate tenant identity, including batch upserts and pgx COPY.
- Use the root package for Retry and SQLSTATE predicates. Use LockMode values for Listing and KeysetListing.
- See [MIGRATION.md](docs/MIGRATION.md) for the old and new call sites.

### Additions

- Table and view iterators, shared and nonblocking row locks, chunked UpsertMany, SetExpr, and read-only Direct pools in both drivers.
- Both drivers support integer, float and boolean array columns alongside string and bigint arrays. Host addresses and network prefixes use net/netip.
- Go 1.26/1.27 and PostgreSQL 14–17 CI, coverage, lint, fuzz and vulnerability checks, a multitenant demo, a cross-library benchmark module, community templates and release tooling.

- **Conditional, upsert, delete & batch writes**: `Rows.UpdateWhere`, `Rows.SetWhere`,
  `Table.DefineUpsert` / `TryDefineUpsert` + `Rows.Upsert` (forbids updating key, tenant,
  and `once` columns; registers SQL for `sqlcheck`), opt-in physical deletes
  (`TableSpec.AllowDelete` + `Rows.Delete` / `Rows.DeleteWhere`), `TableSpec.ReadOnly` mode
  (`ErrReadOnly`), `Rows.InsertReturning`, `Rows.SaveReturning`, and chunked multi-row
  `Rows.InsertMany`.
- **Security & RLS tooling**: `GuardNoSessionEscape` (`ErrForbiddenStatement`) blocking
  `SET ROLE`/`RESET ROLE`/`SESSION AUTHORIZATION`/`set_config` in adapter SQL;
  `Table.RequiresFunction` and `View.Requires`/`RequiresFunction` for `DELETE` and
  function `EXECUTE` privilege checks; `sqlcheck.CheckRLS` and `tabula check-rls` CLI.
- **Views & materialized views in `sqlcheck`**: `ViewsToDefinitions`, `RenderAllSnapshot`,
  `VerifyViewStatements`, `VerifyViewSchema` (supports PostgreSQL regular views and
  materialized views via `pg_attribute` fallback).
- **GoDoc examples & repo standards**: runnable `Example*` tests, `CONTRIBUTING.md`, and
  `SECURITY.md`.
- **Composite keys & configurable soft delete**: comparable struct `K` with tuple `IN`
  for `GetMany` / `LockMany`; `*time.Time` (`IS NULL`), `bool` (`= FALSE`), and
  string/integer status values via `TableSpec.SoftDelete`.
- **Queries & views**: `Rows.ListKeyset` cursor pagination, `Listing.Count`
  (`CountExact`, `CountNone`, `CountEstimate`), `Filter.AndIf` / `In` / `Range`, and
  `View` nested struct prefixes with `View.SelectAs`.
- **Drivers, sessions & transactions**:
  - `stdsql` (stdlib only): `Runner`, `Session`, `ScopePolicy` (RLS `SetLocalConfig` /
    `SetLocalRole` + `GuardQuery`), `Savepoint` and read-only `Direct` pools.
  - `pgxdriver` (separate Go module): `pgx/v5` pool `Runner`, `pgx.Tx` `Session`,
    `ScopePolicy`, `Savepoint`, `CopyFrom` (`COPY` wire-protocol bulk insert) and array converters.
- **Performance, fuzzing & benchmarks**: static struct-offset field accessors and reused
  scan buffers on hot paths; fuzz targets for tag parsing, `Filter`, `OrderSpec`, and
  `sqlcheck.ParseSnapshot`; microbenchmarks and real-PostgreSQL benchmarks.
- **Errors & observability**: `SQLState`, `ClassifyError`, `ErrorClass`, sentinels
  (`ErrUnique`, `ErrForeignKey`, `ErrCheck`, `ErrNotNull`, `ErrSerialization`,
  `ErrDeadlock`, `ErrCanceled`), `ErrorPolicy.Sanitize`, opt-in `log/slog`
  (`LogConfig`, `LogLevelVar`, `Registry.SetLogLevel`, `ScopeLog`, `WithLogger`,
  `db:"...,secret"`, `Quiet()`, `Secret()`), and `Tracer` / `Metrics` hooks.
- **Verification & docs**: `sqlcheck.ParseSnapshot`, `cmd/tabula` CLI
  (`tabula check --dsn ... --snapshot ... [--role ...]`), and `docs/COOKBOOK.md`.

## v0.2.0 — 2026-11-24

First public release, extracted from a production refactoring project.

- `Table[T, K, S]` and `Rows`: get, lock, insert, save, set, soft delete, count, list,
  find, exists, `GetMany`, `LockMany` (key order), `LockBy`.
- `View[T, S]`: decode rows of hand-written SQL by `db` tags.
- `Filter`, `OrderSpec`, JSON / zero / array codecs, privilege derivation.
- `pqdriver`: lib/pq SQLSTATE helpers and array converters.
- `sqlcheck`: SQL snapshots, schema and role-grant checks, `PrepareStatement` and
  `VerifyStatements` (PREPARE plus EXPLAIN EXECUTE).
