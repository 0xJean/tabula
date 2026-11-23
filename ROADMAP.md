# Roadmap

Every release is validated on real adapters of the project it was extracted from before
it is tagged. Items keep the red lines in [docs/DESIGN.md](docs/DESIGN.md).

## v0.2 — current

- `Table[T, K, S]` / `Rows`: get, lock, insert, save, set, soft delete, count, list, find,
  exists; `GetMany`, `LockMany` (ordered by key), `LockBy`.
- `View[T, S]`: decode rows of hand-written SQL by `db` tags; `Cols`, `ColsAs`,
  `Statement`, `One`, `Select`, `Scan`.
- `Filter`, `OrderSpec`, JSON / zero / array codecs, privilege derivation.
- `pqdriver`, `sqlcheck`.

## v0.3 — single-table writes and errors

- Conditional update `UpdateWhere` / `SetWhere`: key plus extra condition, reports whether
  a row matched; zero rows is not an error.
- Controlled upsert: declared conflict and update columns; key, scope and `once` columns
  can never be updated.
- `InsertReturning` / `SaveReturning`: read database-generated columns back into the row.
- `InsertMany` with multi-row VALUES.
- Composite keys (struct `K`).
- Configurable soft delete (timestamp, status value, boolean).
- Error classification: unique, foreign key, check, not null, serialization failure,
  deadlock, cancellation.
- `sqlcheck` as a command-line tool for CI (`pgtable check --dsn ...`).
- Logging per [docs/LOGGING.md](docs/LOGGING.md): `log/slog` logger in `RegistryConfig`,
  levels off / errors / slow / all with run-time switch, fixed record keys, values never
  logged by default, `secret` column tag, `Quiet()` and `Secret()` statement options.

## v0.4 — drivers, sessions and queries

- Separate modules: `stdsql` (`*sql.DB` / `*sql.Tx` sessions), `pgxdriver` (`pgx/v5`).
- Transaction helpers: read-only, isolation level, savepoints, explicit retry helper for
  serialization failures and deadlocks.
- Scope as a plug-in: predicate, session variables (`set_config`) and row-level security
  helpers supplied by configuration.
- Keyset pagination on `(sort column, key)`; count strategies (exact, none, estimate).
- `Filter`: `IN`, ranges, optional conditions; `View` nested struct prefixes.

## v0.5 — observability, performance, documentation

- Tracer and metrics interfaces passed in explicitly (logging already in v0.3); statement keys as span names;
  OpenTelemetry adapter as its own module.
- Optional prepared-statement cache; `COPY` for large batch inserts.
- Benchmarks against `database/sql`, sqlx, pgx and sqlc-generated code, published in the
  README.
- Godoc examples and a cookbook: single-table CRUD, join projection, conditional update,
  batch locking, scope plug-in, migration-tool integration (goose, atlas, golang-migrate).

## v1.0 — stable

- API freeze and compatibility promise.
- Test matrix: PostgreSQL 13–17, the two latest Go releases, `-race`, fuzzing of tag
  parsing, `Filter` and `OrderSpec`, coverage threshold.
- LICENSE, CONTRIBUTING, SECURITY, CHANGELOG, issue and PR templates, automated releases.

## Out of scope

Generated joins or preloading, AutoMigrate or DDL generation, global hooks or callbacks,
databases other than PostgreSQL.
