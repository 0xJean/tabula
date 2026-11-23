# Changelog

All notable changes are listed here. The project follows semantic versioning; before
v1.0 a minor version may contain breaking changes.

## v0.3.0 — 2026-11-24

Complete single-table write, query, driver, session, observability, and CI CLI release.

- **Conditional, upsert & batch writes**: `Rows.UpdateWhere`, `Rows.SetWhere`,
  `Table.DefineUpsert` / `TryDefineUpsert` + `Rows.Upsert` (forbids updating key, tenant,
  and `once` columns; registers SQL for `sqlcheck`), `Rows.InsertReturning`,
  `Rows.SaveReturning`, and chunked multi-row `Rows.InsertMany`.
- **Composite keys & configurable soft delete**: comparable struct `K` with tuple `IN`
  for `GetMany` / `LockMany`; `*time.Time` (`IS NULL`), `bool` (`= FALSE`), and
  string/integer status values via `Table.SoftDelete`.
- **Queries & views**: `Rows.ListKeyset` cursor pagination, `Listing.Count`
  (`CountExact`, `CountNone`, `CountEstimate`), `Filter.AndIf` / `In` / `Range`, and
  `View` nested struct prefixes with `View.SelectAs`.
- **Drivers, sessions & transactions**:
  - `stdsql` (stdlib only): `Runner`, `Session`, `ScopePolicy` (RLS `SetLocalConfig` /
    `SetLocalRole` + `GuardQuery`), `Savepoint`, and `Retry` for `40001` / `40P01`.
  - `pgxdriver` (separate Go module): `pgx/v5` pool `Runner`, `pgx.Tx` `Session`,
    `ScopePolicy`, `Savepoint`, `CopyFrom` (`COPY` wire-protocol bulk insert), SQLSTATE
    helpers, and array converters.
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
