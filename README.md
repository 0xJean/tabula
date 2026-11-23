<p align="center"><img src="docs/brand/lockup-light.png" alt="Tabula" width="420"></p>

# Tabula

[![CI](https://github.com/0xJean/tabula/actions/workflows/ci.yml/badge.svg)](https://github.com/0xJean/tabula/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/0xJean/tabula.svg)](https://pkg.go.dev/github.com/0xJean/tabula) [![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Declarative descriptors, explicit SQL, verified against a real PostgreSQL.

Tabula describes PostgreSQL tables and result rows with Go struct tags, derives the
plain single-table statements from them, decodes the rows of hand-written SQL, and
checks every registered statement against a real database. It is not an ORM: it never
builds joins, never generates DDL, and never runs hidden callbacks.

> The latest tagged core release is v0.2.0. This checkout contains the development API for v0.3. See the [migration guide](docs/MIGRATION.md) and [changelog](CHANGELOG.md).

## Try it locally

From a checkout with Go 1.26 or later and Docker Compose installed, run:

```sh
cd examples/multitenant
docker compose up -d --wait && go run .
```

The example verifies the schema, executes scoped writes and shows what each tenant can read through a PostgreSQL RLS policy. [Example and expected output](examples/multitenant/README.md)

## What it does

| Need | Tabula |
| --- | --- |
| Single-table CRUD | `Define(reg, TableSpec[T, K]{…})` derives get, lock, insert, save, set, soft/physical delete, count and list from `db` tags (`key`, `serial`, `tenant`, `deleted`, `version`, `once`, `json`, `zero`, `array`, `secret`), including composite struct keys and timestamp/bool/status soft delete |
| Conditional, upsert, delete & batch writes | `UpdateWhere`, `SetWhere`, `DefineUpsert`/`Upsert`, `Delete`/`DeleteWhere`, `InsertReturning`, `SaveReturning`, `InsertMany`, `UpsertMany`, `SetExpr` |
| Rows of hand-written SQL (joins, views, materialized views) | `DefineView(reg, ViewSpec[T]{…})` with nested struct prefixes, `SelectAs`, `Requires`, and `RequiresFunction`: you write the SQL, Tabula decodes the rows |
| Batch, locked & keyset reads | `GetMany`, `LockMany` (fixed key order), `Iter`, `LockBy`, `LockMode` (including `SKIP LOCKED` and `NOWAIT`), `ListKeyset`, `CountExact`/`CountNone`/`CountEstimate` |
| Safe dynamic parts & guards | `Filter` (`And`, `AndIf`, `In`, `Range`, `??` escape), `OrderSpec` (whitelisted sort columns), `GuardNoSessionEscape` (blocks `SET ROLE`/`set_config`) |
| Sessions, scope & retries | `stdsql` (`*sql.DB`/`*sql.Tx` Runner, `ScopePolicy` RLS plug-in, `Savepoint`, read-only `Direct` pool) and `pgxdriver` (`pgx/v5` Runner, Session, Direct & binary COPY), with driver-independent `tabula.Retry` |
| Error classification | Distinct PostgreSQL sentinels & classes (`unique`, `foreign_key`, `check`, `not_null`, `serialization`, `deadlock`, `canceled`) |
| Observability | Opt-in `log/slog` ([spec](docs/LOGGING.md)), `Tracer`, and `Metrics` interfaces on `RegistryConfig` |
| Verification & CI CLI | `sqlcheck` (`VerifyStatements`, `VerifyViewStatements`, `VerifySchema` incl. materialized views, `CheckRolePrivileges` incl. `DELETE` & function `EXECUTE`, `CheckRLS`) + `cmd/tabula` (`tabula check` & `tabula check-rls`) |

## Benchmarks

The [comparison benchmark](benchmarks/README.md) runs the same 100-row SELECT through Tabula, database/sql, pgx, sqlx and GORM. It reports latency, allocated bytes, allocations and the size of each adapter. Separate microbenchmarks and bulk-write benchmarks are available in the engine and pgxdriver modules.

```sh
scripts/workspace.sh
go test -run='^$' -bench=. -benchmem .
TABULA_TEST_DSN='postgres://...' go test -tags=integration -run='^$' -bench='^BenchmarkPG_' -benchmem .
(cd pgxdriver && TABULA_TEST_DSN='postgres://...' go test -tags=integration -run='^$' -bench='^BenchmarkPGX_' -benchmem .)
```

<details>
<summary>Earlier transaction and bulk-write measurements</summary>

These measurements were recorded before the bound-scope API change, on Apple M4 Pro with Go 1.27 and local PostgreSQL 15. The SELECT benchmarks below include a transaction per operation.

| Benchmark | Time/op | Bytes/op | Allocs/op |
| --- | ---: | ---: | ---: |
| `Select` 100 rows — raw `database/sql` | 793 µs | 30.4 KB | 647 |
| `Select` 100 rows — `tabula` + `stdsql` | 771 µs | 32.3 KB | 755 |
| `Insert` 1,000 rows — `tabula` `InsertMany` (`stdsql`) | 4.83 ms | 1,141 KB | 14,977 |
| `Insert` 1,000 rows — `pgxdriver.CopyFrom` (`COPY`) | 3.92 ms | 485 KB | 8,075 |
| Pure engine overhead: `Rows.Get` (in-memory) | 132 ns | 288 B | 6 |
| Pure engine overhead: `OrderSpec.Resolve` | 65 ns | 48 B | 3 |

</details>

Fuzzing targets are included for `db` tag parsing, `Filter`, `OrderSpec`, and `sqlcheck.ParseSnapshot`:

```sh
go test -fuzz=FuzzParseColumnTag -fuzztime=5s .
go test -fuzz=FuzzFilterAnd -fuzztime=5s .
go test -fuzz=FuzzOrderSpecResolve -fuzztime=5s .
go test -fuzz=FuzzParseSnapshot -fuzztime=5s ./sqlcheck
```

## Install a tagged release

```sh
go get github.com/0xJean/tabula@v0.2.0
```

For the development API and pgxdriver, run `scripts/workspace.sh` from the repository root. Go then uses this checkout for all three modules. [Local development and testing](CONTRIBUTING.md)

## What it will not do

- Joins, preloading or lazy loading generated from struct relations.
- AutoMigrate or any DDL generation. Migrations stay the source of truth.
- Global registries, implicit hooks or callbacks. Everything is passed in explicitly.

## Layout

| Package | Role | Dependencies |
| --- | --- | --- |
| `tabula` | Engine: registry, tables, views, rows, writes, upserts, keyset, filter, order, errors, logging, tracing/metrics interfaces | standard library only |
| `tabula/stdsql` | `*sql.DB` / `*sql.Tx` sessions, `Runner`, `ScopePolicy` (RLS), `Savepoint`, read-only `Direct` pool | standard library only |
| `tabula/sqlcheck` | Snapshot render/parse, schema, privilege, and PREPARE/EXPLAIN verification | standard library only |
| `tabula/pqdriver` | `lib/pq` array converters | `github.com/lib/pq` |
| `tabula/pgxdriver` | Separate module: `pgx/v5` pool Runner, `pgx.Tx` Session, Direct & array converters | `github.com/jackc/pgx/v5` |
| `tabula/cmd/tabula` | CI CLI (`tabula check --dsn ... --snapshot ... [--role ...]`) | `sqlcheck`, `github.com/lib/pq` |

Applications may use `stdsql` or `pgxdriver` directly, or wrap `tabula` in a thin
binding package of their own to enforce compile-time session/scope invariants.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
