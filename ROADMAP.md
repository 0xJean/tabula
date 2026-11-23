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

## v0.3 — complete feature release (current)

- **Single-table writes**: `UpdateWhere` / `SetWhere` (0 rows is not an error),
  controlled upsert (`DefineUpsert` / `Upsert`; key, scope and `once` columns can never
  be updated), `InsertReturning` / `SaveReturning`, multi-row `InsertMany` (chunked at
  PostgreSQL's 65,535 param limit).
- **Schema shapes**: composite keys (struct `K`) with tuple `IN` for `GetMany` / `LockMany`;
  configurable soft delete (`*time.Time`, `bool`, or status value via `Table.SoftDelete`).
- **Queries & projections**: keyset pagination (`ListKeyset`), count strategies
  (`CountExact`, `CountNone`, `CountEstimate`), `Filter` (`AndIf`, `In`, `Range`, `??`),
  `View` nested struct prefixes and `SelectAs`.
- **Drivers, sessions & transactions**: `stdsql` (`*sql.DB` / `*sql.Tx` Runner,
  `ScopePolicy` RLS plug-in, `Savepoint`, serialization/deadlock `Retry`) and separate
  `pgxdriver` (`pgx/v5`) module including `CopyFrom` binary `COPY` bulk inserts.
- **Errors & observability**: 7 PostgreSQL error classes and sentinels, opt-in `log/slog`
  per [docs/LOGGING.md](docs/LOGGING.md) (`LogOff`/`LogErrors`/`LogSlow`/`LogAll`,
  atomic level switch, `secret` tag, `Quiet()`/`Secret()`), plus explicit `Tracer` and
  `Metrics` hooks on `RegistryConfig`.
- **Verification, fuzzing, benchmarks & docs**: `sqlcheck.ParseSnapshot`, `cmd/tabula`
  CI CLI (`tabula check --dsn ... --snapshot ...`), static-offset field accessors, fuzz
  targets (`FuzzParseColumnTag`, `FuzzFilterAnd`, `FuzzOrderSpecResolve`,
  `FuzzParseSnapshot`), micro/real-PG benchmarks, and [docs/COOKBOOK.md](docs/COOKBOOK.md).

## v1.0 — stability and ecosystem adapters

- API freeze and semver compatibility promise.
- Optional OpenTelemetry / Prometheus adapter modules over `Tracer` and `Metrics`.
- Optional prepared-statement cache.
- `CONTRIBUTING.md`, `SECURITY.md`, issue/PR templates, automated release workflow.

## Out of scope

Generated joins or preloading, AutoMigrate or DDL generation, global hooks or callbacks,
databases other than PostgreSQL.
