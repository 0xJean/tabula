# Roadmap

Every release is validated against real PostgreSQL (14–17) before it is tagged.
Items keep the red lines in [docs/DESIGN.md](docs/DESIGN.md).

## v0.2 — released

- `Table[T, K, S]` / `Rows`: get, lock, insert, save, set, soft delete, count, list, find,
  exists; `GetMany`, `LockMany` (ordered by key), `LockBy`.
- `View[T, S]`: decode rows of hand-written SQL by `db` tags; `Cols`, `ColsAs`,
  `Statement`, `One`, `Select`, `Scan`.
- `Filter`, `OrderSpec`, JSON / zero / array codecs, privilege derivation.
- `pqdriver`; `sqlcheck`: snapshots, schema columns, privilege derivation and role grants,
  `PrepareStatement` / `VerifyStatements` (PREPARE plus EXPLAIN EXECUTE on a real database).

## v0.3 — in development

- **Immutable descriptors & generic row scope**: `TableSpec` / `ViewSpec`, bound-scope
  sessions (`Scope` / `Unscoped` / `db:"...,scope"`), driver-independent `tabula.Retry` and
  SQLSTATE helpers.
- **Single-table writes & table modes**: `UpdateWhere` / `SetWhere` (0 rows is not an
  error), controlled upsert (`DefineUpsert` / `Upsert` / `UpsertMany`; key, scope and `once`
  columns can never be updated), `SetExpr` (`Add`, `SetNow`), opt-in physical deletes
  (`TableSpec.AllowDelete` + `Delete` / `DeleteWhere`), `TableSpec.ReadOnly` mode,
  `InsertReturning` / `SaveReturning`, multi-row `InsertMany` (chunked at PostgreSQL's
  65,535 param limit).
- **Schema shapes & codecs**: composite keys (struct `K`) with tuple `IN` for `GetMany` /
  `LockMany`; configurable soft delete (`*time.Time`, `bool`, or status value via
  `TableSpec.SoftDelete`); built-in `net/netip` and `int32`/`float64`/`bool` array codecs.
- **Queries, streaming & projections**: `iter.Seq2` streaming (`Iter`, `SelectIter`),
  `LockMode` (`FOR UPDATE`, `SKIP LOCKED`, `NOWAIT`, `FOR SHARE`), keyset pagination
  (`ListKeyset`), count strategies (`CountExact`, `CountNone`, `CountEstimate`), `Filter`
  (`AndIf`, `In`, `Range`, `??`), `View` nested struct prefixes and `SelectAs`.
- **Drivers, sessions & transactions**: `stdsql` (`*sql.DB` / `*sql.Tx` Runner,
  `ScopePolicy` RLS plug-in, `Savepoint`, read-only `Direct` pools) and separate
  `pgxdriver` (`pgx/v5`) module including `Direct` pools and `CopyFrom` binary `COPY`.
- **Errors & observability**: PostgreSQL error classes and sentinels, opt-in `log/slog`
  per [docs/LOGGING.md](docs/LOGGING.md) (`LogOff`/`LogErrors`/`LogSlow`/`LogAll`,
  atomic level switch, `secret` tag, `Quiet()`/`Secret()`), plus explicit `Tracer` and
  `Metrics` hooks on `RegistryConfig`.
- **Verification, quality gates & docs**: `sqlcheck.ParseSnapshot`, `RenderAllSnapshot`,
  `VerifyViewStatements`, `VerifyViewSchema` (regular & materialized views), `DELETE` &
  function `EXECUTE` privilege checks, `CheckRLS` + `cmd/tabula` CLI (`tabula check` &
  `tabula check-rls`), `GuardNoSessionEscape` & `GuardReadOnly`, `.golangci.yml`,
  `scripts/check.sh`, `AGENTS.md`, [docs/TESTING.md](docs/TESTING.md),
  [docs/MIGRATION.md](docs/MIGRATION.md), [docs/COOKBOOK.md](docs/COOKBOOK.md),
  [examples/multitenant](examples/multitenant/README.md), and
  [benchmarks](benchmarks/README.md).

### PostgreSQL authorization and RLS verification

- Collect static and dynamic handwritten SQL sites, review exceptions and verify explicit per-role workloads.
- Compare direct role grants against a manifest. Trial individual revocations in a disposable database and produce reviewable GRANT and REVOKE SQL.
- Snapshot policies, FORCE RLS tables, grants and SECURITY DEFINER routines. Classify policies with application rules, check command coverage and exercise a role/scope/statement matrix.
- Distinguish RLS and privilege denials, configure session guards and clone content-addressed PostgreSQL test templates through a chosen migration. [Design and configuration](docs/AUTHORIZATION.md)

## v1.0 — stability and ecosystem adapters

- API freeze and semver compatibility promise.
- Optional OpenTelemetry / Prometheus adapter modules over `Tracer` and `Metrics`.
- Optional prepared-statement cache.

## Out of scope

Generated joins or preloading, AutoMigrate or DDL generation, global hooks or callbacks,
databases other than PostgreSQL.
