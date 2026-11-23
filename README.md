<p align="center"><img src="docs/brand/lockup-light.png" alt="Tabula" width="420"></p>

# Tabula

Declarative descriptors, explicit SQL, verified against a real PostgreSQL.

Tabula describes PostgreSQL tables and result rows with Go struct tags, derives the
plain single-table statements from them, decodes the rows of hand-written SQL, and
checks every registered statement against a real database. It is not an ORM: it never
builds joins, never generates DDL, and never runs hidden callbacks.

> Status: v0.3, pre-1.0. The API may still change between minor versions. The engine was
> extracted from a production refactoring project. See [ROADMAP.md](ROADMAP.md),
> [docs/DESIGN.md](docs/DESIGN.md), and [docs/COOKBOOK.md](docs/COOKBOOK.md).

## What it does

| Need | Tabula |
| --- | --- |
| Single-table CRUD | `Define(Table[T, K, S]{Name, Order})` derives get, lock, insert, save, set, soft delete, count and list from `db` tags (`key`, `serial`, `tenant`, `deleted`, `version`, `once`, `json`, `zero`, `array`, `secret`), including composite struct keys and timestamp/bool/status soft delete |
| Conditional, upsert & batch writes | `UpdateWhere`, `SetWhere`, `DefineUpsert`/`Upsert`, `InsertReturning`, `SaveReturning`, multi-row `InsertMany` |
| Rows of hand-written SQL (joins, projections, reports) | `DefineView(View[T, S]{...})` with nested struct prefixes and `SelectAs`: you write the SQL, Tabula decodes the rows |
| Batch, locked & keyset reads | `GetMany`, `LockMany` (fixed key order), `LockBy`, `ListKeyset`, `CountExact`/`CountNone`/`CountEstimate` |
| Safe dynamic parts | `Filter` (`And`, `AndIf`, `In`, `Range`, `??` escape), `OrderSpec` (whitelisted sort columns) |
| Sessions, scope & retries | `stdsql` (`*sql.DB`/`*sql.Tx` Runner, `ScopePolicy` RLS plug-in, `Savepoint`, `Retry` for `40001`/`40P01`) and `pgxdriver` (`pgx/v5` Runner, Session & `CopyFrom` binary `COPY`) |
| Error classification | Distinct PostgreSQL sentinels & classes (`unique`, `foreign_key`, `check`, `not_null`, `serialization`, `deadlock`, `canceled`) |
| Observability | Opt-in `log/slog` ([spec](docs/LOGGING.md)), `Tracer`, and `Metrics` interfaces on `RegistryConfig` |
| Verification & CI CLI | `sqlcheck` + `cmd/tabula` (`tabula check --dsn ... --snapshot ... [--role ...]`) PREPARE and EXPLAIN every registered statement, compare columns with the schema, derive privileges, snapshot the SQL for review |

## Benchmarks

Measured on Apple M4 Pro (Go 1.27, local PostgreSQL 15, `-benchmem`):

| Benchmark | Time/op | Bytes/op | Allocs/op |
| --- | ---: | ---: | ---: |
| `Select` 100 rows — raw `database/sql` | 793 µs | 30.4 KB | 647 |
| `Select` 100 rows — `tabula` + `stdsql` | 771 µs | 32.3 KB | 755 |
| `Insert` 1,000 rows — `tabula` `InsertMany` (`stdsql`) | 4.83 ms | 1,141 KB | 14,977 |
| `Insert` 1,000 rows — `pgxdriver.CopyFrom` (`COPY`) | 3.92 ms | 485 KB | 8,075 |
| Pure engine overhead: `Rows.Get` (in-memory) | 132 ns | 288 B | 6 |
| Pure engine overhead: `OrderSpec.Resolve` | 65 ns | 48 B | 3 |

Reproduce locally:

```sh
go test -run='^$' -bench=. -benchmem .
TABULA_TEST_DSN='postgres://...' go test -tags=integration -run='^$' -bench='^BenchmarkPG_' -benchmem .
(cd pgxdriver && TABULA_TEST_DSN='postgres://...' go test -tags=integration -run='^$' -bench='^BenchmarkPGX_' -benchmem .)
```

Fuzzing targets are included for `db` tag parsing, `Filter`, `OrderSpec`, and `sqlcheck.ParseSnapshot`:

```sh
go test -fuzz=FuzzParseColumnTag -fuzztime=5s .
go test -fuzz=FuzzFilterAnd -fuzztime=5s .
go test -fuzz=FuzzOrderSpecResolve -fuzztime=5s .
go test -fuzz=FuzzParseSnapshot -fuzztime=5s ./sqlcheck
```

## Install

```sh
go get github.com/0xJean/tabula@v0.3.0
# Optional pgx/v5 driver module:
go get github.com/0xJean/tabula/pgxdriver@v0.3.0
```

Requires Go 1.27 or later and PostgreSQL. Unit tests need no database; integration
tests need a disposable one:

```sh
go test -race ./...
TABULA_TEST_DSN='postgres://user:pass@localhost:5432/db?sslmode=disable' \
  go test -race -tags=integration ./...
```

## What it will not do

- Joins, preloading or lazy loading generated from struct relations.
- AutoMigrate or any DDL generation. Migrations stay the source of truth.
- Global registries, implicit hooks or callbacks. Everything is passed in explicitly.

## Layout

| Package | Role | Dependencies |
| --- | --- | --- |
| `tabula` | Engine: registry, tables, views, rows, writes, upserts, keyset, filter, order, errors, logging, tracing/metrics interfaces | standard library only |
| `tabula/stdsql` | `*sql.DB` / `*sql.Tx` sessions, `Runner`, `ScopePolicy` (RLS), `Savepoint`, serialization/deadlock `Retry` | standard library only |
| `tabula/sqlcheck` | Snapshot render/parse, schema, privilege, and PREPARE/EXPLAIN verification | standard library only |
| `tabula/pqdriver` | `lib/pq` SQLSTATE helpers and array converters | `github.com/lib/pq` |
| `tabula/pgxdriver` | Separate module: `pgx/v5` pool Runner, `pgx.Tx` Session, SQLSTATE & array converters | `github.com/jackc/pgx/v5` |
| `tabula/cmd/tabula` | CI CLI (`tabula check --dsn ... --snapshot ... [--role ...]`) | `sqlcheck`, `github.com/lib/pq` |

Applications may use `stdsql` or `pgxdriver` directly, or wrap `tabula` in a thin
binding package of their own to enforce compile-time session/scope invariants.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
