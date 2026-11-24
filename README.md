<p align="center"><img src="docs/brand/lockup-light.png" alt="Tabula" width="420"></p>

# Tabula

[![CI](https://github.com/0xJean/tabula/actions/workflows/ci.yml/badge.svg)](https://github.com/0xJean/tabula/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/0xJean/tabula.svg)](docs/REFERENCE.md) [![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Declarative descriptors, explicit SQL, verified against a real PostgreSQL.

Tabula describes PostgreSQL tables and result rows with Go struct tags, derives the
plain single-table statements from them, decodes the rows of hand-written SQL, and
checks every registered statement against a real database. It is not an ORM: it never
builds joins, never generates DDL, and never runs hidden callbacks.

**Integrating v0.3?** Start with the [application integration guide](docs/INTEGRATION.md). It maps each task to its package or command. The [complete Go API and CLI reference](docs/REFERENCE.md) is generated from this checkout.

Read the README at the Git tag or commit your application builds. The [migration guide](docs/MIGRATION.md) covers upgrading to v0.3. The [changelog](CHANGELOG.md) records release status.

## Try it locally

From a checkout with Go 1.26 or later and Docker Compose installed, run:

```sh
cd examples/multitenant
docker compose up -d --wait && go run .
```

The example verifies the schema, executes scoped writes and shows what each tenant can read through a PostgreSQL RLS policy. [Example and expected output](examples/multitenant/README.md)

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

CI fuzzes `db` tag parsing, `Filter`, `OrderSpec`, `GuardNoSessionEscape`, and `sqlcheck.ParseSnapshot`.

```sh
go test -fuzz=FuzzParseColumnTag -fuzztime=5s .
go test -fuzz=FuzzFilterAnd -fuzztime=5s .
go test -fuzz=FuzzOrderSpecResolve -fuzztime=5s .
go test -fuzz=FuzzGuardNoSessionEscape -fuzztime=5s .
go test -fuzz=FuzzParseSnapshot -fuzztime=5s ./sqlcheck
```

## Working from the v0.3 checkout

Run `scripts/workspace.sh` from the Tabula repository root to use its three local modules together. In an application repository, check the selected module with `go list -m -json github.com/0xJean/tabula`. [Version selection and adapter setup](docs/INTEGRATION.md)

## What it will not do

- Joins, preloading or lazy loading generated from struct relations.
- AutoMigrate or any DDL generation. Migrations stay the source of truth.
- Global registries, implicit hooks or callbacks. Everything is passed in explicitly.

## Layout

| Package | Role | Dependencies |
| --- | --- | --- |
| `tabula` | Engine: registry, tables, views, rows, writes, upserts, keyset, filter, order, errors, logging, tracing/metrics interfaces | standard library only |
| `tabula/stdsql` | `*sql.DB` / `*sql.Tx` sessions, `Runner`, `ScopePolicy` (RLS), `Savepoint`, read-only `Direct` pool | standard library only |
| `tabula/sqlcheck` | Descriptor and catalog snapshots, source SQL inventory, role workloads, grant minimization and RLS checks | standard library only |
| `tabula/pqdriver` | `lib/pq` array converters | `github.com/lib/pq` |
| `tabula/pgxdriver` | Separate module: `pgx/v5` pool Runner, `pgx.Tx` Session, Direct, binary COPY and array converters | `github.com/jackc/pgx/v5` |
| `tabula/postgresfixture` | Content-addressed installation templates, migration prefixes and isolated per-test databases | standard library only |
| `tabula/cmd/tabula` | SQL verification (`tabula check`), RLS audit (`tabula check-rls`) and role workload tools. [All commands and flags](docs/REFERENCE.md#cli) | `sqlcheck`, `github.com/lib/pq` |

Applications may use `stdsql` or `pgxdriver` directly, or wrap `tabula` in a thin
binding package of their own to enforce compile-time session/scope invariants.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
