<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/lockup-dark.png">
    <img src="docs/brand/lockup-light.png" alt="Tabula" width="420">
  </picture>
</p>

# Tabula

[![CI](https://github.com/0xJean/tabula/actions/workflows/ci.yml/badge.svg)](https://github.com/0xJean/tabula/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/0xJean/tabula.svg)](https://pkg.go.dev/github.com/0xJean/tabula) [![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Declarative descriptors, explicit SQL, verified against a real PostgreSQL.

Tabula describes PostgreSQL tables and result rows with Go struct tags, binds every query to an explicit session and role, and verifies registered statements, types, grants and row-level security against a real PostgreSQL instance.

## Choose an entry point

Start with the lightest tier that fits the query. All three share one `*tabula.Registry[S]`, `SQLAuthority`, `SessionGuard`, observability hooks and `sqlcheck` pipeline.

| Tier | Package | Use when | Guide |
| --- | --- | --- | --- |
| **1. `Table` CRUD** | `tabula` | Single-table primary-key reads, locks, inserts, optimistic updates, soft deletes, controlled upserts and binary `COPY`. | [Quick start](#read-a-row-by-primary-key) · [Cookbook](docs/COOKBOOK.md#2-single-table-crud-with-optimistic-locking-and-once-protection) |
| **2. Named-parameter SQL (`DefineSQL*`)** | `tabula` | Most application queries and writes: joins, CTEs, aggregates, dynamic lists, `RETURNING` writes and advisory locks written as explicit `@name` PostgreSQL SQL with `[Row, Args]` structs. | [Choosing an API](docs/CHOOSING-AN-API.md) · [Named-parameter SQL](docs/COOKBOOK.md#named-parameter-sql-with-struct-scanning) |
| **3. Typed Plan AST (`package plan`)** | `tabula/plan` | Programmatic query/write composition, finite filter/order/pagination families, catalog-proven keyset cursors, or column-level privilege proofs derived directly from an AST. | [Choosing an API](docs/CHOOSING-AN-API.md) · [Expressions](docs/EXPRESSIONS.md) · [Composition](docs/QUERY-COMPOSITION.md) · [Typed writes](docs/TYPED-WRITES.md) |

See [Choosing an API](docs/CHOOSING-AN-API.md) for a one-page task matrix and copy-pasteable starter binding, [Integration guide](docs/INTEGRATION.md) for every task and CLI command, [Authorization & sqlcheck](docs/AUTHORIZATION.md) for role-bound verification and table- or column-level grant auditing, [Migration guide](docs/MIGRATION.md) for upgrades, [Changelog](CHANGELOG.md) for release history, and [Go API & CLI reference](docs/REFERENCE.md) for the exact revision reference.

## Install v0.6.0

Run `go get github.com/0xJean/tabula@v0.6.0` in your application module with Go 1.26 or later. For the native driver and telemetry modules, use the matching version with `go get github.com/0xJean/tabula/pgxdriver@v0.6.0` and/or `go get github.com/0xJean/tabula/oteltabula@v0.6.0`.

## Read a row by primary key

Suppose `public.items` has a `bigint` primary key named `id` and a non-null `text` column named `name`. Import `github.com/0xJean/tabula` and `github.com/0xJean/tabula/stdsql`.

```go
type Item struct {
	ID   int64  `db:"id,key"`
	Name string `db:"name"`
}

// Define the table once at startup.
reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
items := tabula.Define(reg, tabula.TableSpec[Item, int64]{Name: "public.items"})

// In a request, use the caller's context and open *sql.Tx.
session := stdsql.NewSession(tx, struct{}{}, stdsql.NoScopePolicy())
item, err := items.Bind(session).Get(ctx, 42)
```

`Get` returns a pointer to the matching `Item`, or `(nil, nil)` if the key is missing. When streaming with `Iter`, finish the loop or break before using the transaction for another lookup. Overlapping operations return `tabula.ErrSessionBusy`. [Streaming example](docs/COOKBOOK.md#10-stream-a-worker-batch)

For a role that only updates selected columns or only appends rows, set `TableSpec.WriteMode` to derive the matching SQL and grants. [Table write modes](docs/AUTHORIZATION.md#table-write-modes)

## Try it locally

From a checkout with Go 1.26 or later and Docker Compose installed, run:

```sh
cd examples/multitenant
docker compose up -d --wait && go run .
```

The example verifies the schema, executes scoped writes and shows what each tenant can read through a PostgreSQL RLS policy. [Example and expected output](examples/multitenant/README.md)

## Benchmarks

The comparison benchmark reads the same 100 rows with Tabula, database/sql, pgx, sqlx and GORM. The results table lists median latency, bytes and allocations per operation from five runs, along with each adapter's line count. [Results, test environment and methodology](benchmarks/README.md)

Run these commands from the repository root. For the database benchmarks, export `TABULA_TEST_DSN` with the connection string for a disposable PostgreSQL database.

```sh
scripts/workspace.sh
go test -run='^$' -bench=. -benchmem .
go test -tags=integration -run='^$' -bench='^BenchmarkPG_' -benchmem .
go -C pgxdriver test -tags=integration -run='^$' -bench='^BenchmarkPGX_' -benchmem .
go -C benchmarks test -run='^$' -bench='^BenchmarkSelect100$' -benchmem -count=5
```

## Fuzz tests

CI fuzzes `db` tag parsing, `Filter`, `OrderSpec`, `GuardNoSessionEscape`, composed query fragment compilation, named-parameter SQL rewriting and declaration validation (`DefineSQL`), and `sqlcheck.ParseSnapshot`.

```sh
go test -fuzz=FuzzParseColumnTag -fuzztime=5s .
go test -fuzz=FuzzFilterAnd -fuzztime=5s .
go test -fuzz=FuzzOrderSpecResolve -fuzztime=5s .
go test -fuzz=FuzzGuardNoSessionEscape -fuzztime=5s .
go test -fuzz=FuzzCompileQueryFragment -fuzztime=5s .
go test -fuzz=FuzzDefineSQL -fuzztime=5s .
go test -fuzz=FuzzRewriteNamedSQL -fuzztime=5s ./internal/pgstmt
go test -fuzz=FuzzParseSnapshot -fuzztime=5s ./sqlcheck
```

## Working from a local checkout

Run `scripts/workspace.sh` from the Tabula repository root to use all local modules together. In an application repository, check the selected module with `go list -m -json github.com/0xJean/tabula`. [Version selection and adapter setup](docs/INTEGRATION.md)

## What it will not do

- Joins, preloading or lazy loading generated from struct relations.
- AutoMigrate or any DDL generation. Migrations stay the source of truth.
- Global registries, implicit hooks or callbacks. Everything is passed in explicitly.

## Layout

| Package | Role | Dependencies |
| --- | --- | --- |
| `tabula` | Core engine: registry, `Table` CRUD, `View`, named-parameter SQL (`DefineSQL*`), `SQLAuthority`, sealed sessions, `SessionGuard`, capabilities, errors, logging, tracing/metrics interfaces | standard library only |
| `tabula/plan` | Typed Plan AST: scalar, aggregate and window expressions, explicit joins, CTEs, subqueries, row locks, advisory locks, routines and typed writes | standard library only |
| `tabula/pgtype` | PostgreSQL catalog types and input encoders for typed fields and parameters | standard library only |
| `tabula/stdsql` | Role-bound `*sql.DB` / `*sql.Tx` `Runner`, `Shared`, `RawSession`, `ScopePolicy`, `Savepoint`, read-only `Direct` pool | standard library only |
| `tabula/sqlcheck` | Descriptor and catalog snapshots, source SQL & security call-site inventory, role workloads, column- or table-level grant minimization, login-role hygiene and RLS checks | standard library only |
| `tabula/pqdriver` | `lib/pq` array converters | `github.com/lib/pq` |
| `tabula/pgxdriver` | Separate module: role-bound `pgx/v5` pool `Runner`, `Shared`, `RawSession`, `Session`, `Direct`, non-executing `SQLVerifier`, binary COPY and array converters | `github.com/jackc/pgx/v5` |
| `tabula/oteltabula` | Separate module: OpenTelemetry `Tracer` and `Metrics` adapters | `go.opentelemetry.io/otel` |
| `tabula/postgresfixture` | Content-addressed templates, migration prefixes, isolated test databases and idle template cleanup | standard library only |
| `tabula/cmd/tabula` | SQL verification (`tabula check`), RLS audit (`tabula check-rls`), login role audit (`tabula check-login-role`) and role workload tools. [All commands and flags](docs/REFERENCE.md#cli) | `sqlcheck`, `github.com/lib/pq` |

Applications may use `stdsql` or `pgxdriver` directly, or wrap `tabula` in a thin binding package of their own to enforce compile-time session/scope invariants. See [Choosing an API](docs/CHOOSING-AN-API.md#minimal-application-binding) for a starter template and [Authorization](docs/AUTHORIZATION.md#check-a-role-at-startup) for startup readiness and disposable test fixture leases.

## Contributing

See the [contributing guide](CONTRIBUTING.md) for local checks and the [code of conduct](CODE_OF_CONDUCT.md) for community expectations and moderation.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

Design records: [Core architecture](docs/DESIGN.md) · [v0.4 typed plans](docs/DESIGN-v0.4.md) · [M3 composition & writes](docs/DESIGN-M3.md).
