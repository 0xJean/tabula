<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/lockup-dark.png">
    <img src="docs/brand/lockup-light.png" alt="Tabula" width="420">
  </picture>
</p>

# Tabula

[![CI](https://github.com/0xJean/tabula/actions/workflows/ci.yml/badge.svg)](https://github.com/0xJean/tabula/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/0xJean/tabula.svg)](https://pkg.go.dev/github.com/0xJean/tabula) [![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Declarative descriptors, explicit SQL, verified against a real PostgreSQL.

Tabula describes PostgreSQL tables and result rows with Go struct tags, derives the
plain single-table statements from them and decodes the rows of hand-written SQL.
Typed plans register each permitted statement for verification against PostgreSQL.
An explicit join names both relations and describes their match. A view decodes the result.

The integration guide maps each task to its package or command. The API and CLI reference is generated from this checkout. [Integration guide](docs/INTEGRATION.md) · [Go API and CLI reference](docs/REFERENCE.md)

Read the README at the Git tag or commit your application builds. The [migration guide](docs/MIGRATION.md) covers upgrading to v0.5 and earlier releases. The [changelog](CHANGELOG.md) records release status.

A registry can restrict selected runners and Direct pools to the SQL shapes it issued. Pass its `SQLAuthority` to each role that needs this policy. [Registered execution](docs/AUTHORIZATION.md#registered-execution)

For everyday queries and writes without building a full AST, `DefineSQL`, `DefineSQLExec`, `DefineSQLReturning` and `DefineSQLList` register named-parameter (`@name`) PostgreSQL statements directly with `[Row, Args]` Go structs on the same `Registry`, `SQLAuthority`, `SessionGuard` and observability pipeline. Go type families (`RegisterTypeFamily` / `TypeFamilyOf`) derive compatible PostgreSQL types so `sqlcheck` verifies parameter OIDs, exact positional column names, and domain-unwrapped column OIDs against PostgreSQL without executing writes. [Named-parameter SQL examples](docs/COOKBOOK.md#named-parameter-sql-with-struct-scanning)

`sqlcheck` supports both column-level (`GranularityAll`, default) and table-level (`GranularityTable`) grant auditing, minimization, and startup readiness (`ReadinessNeeds.TableSelect`, `TableInsert`, `TableUpdate`, `TableDelete`), plus `--granularity=all|table` on `tabula catalog`, `tabula grants` and `tabula minimize`. [Table-level grant granularity](docs/AUTHORIZATION.md#table-level-grant-granularity)

The [v0.4 design](docs/DESIGN-v0.4.md) describes typed predicates, per-shape verification and the driver interface for registered queries.

Typed scalar expressions cover conditionals, native arithmetic and casts, text and regex, time, JSON/JSONB, arrays, hashes and UUID generation. They compose in reads and conditional assignment values, with exact catalog types and minimum grants. [Expression guide](docs/EXPRESSIONS.md)

`DefineRows`, `DefineUpdateMany` and `DefineDeleteMany` register the exact statements a single-table plan can execute. The inventory expands those statements for each assigned PostgreSQL role. [Registered row plans](docs/COOKBOOK.md#registered-single-table-reads-and-writes)

Typed reads support computed group and order expressions, explicit NULL placement, aggregate filters and ordered string, array and JSON aggregation. `ReadSelect.Distinct` removes duplicate projections; `DistinctOn` selects representatives with a complete winner order. [Grouping and aggregate examples](docs/COOKBOOK.md#computed-groups-and-aggregate-inputs)

Use `DefinePlanSource` for a physical table accessed exclusively through typed plans. Call `MapTable(items).As("i")` to reuse its fields in reads, writes and joins. `DefineTableRows` returns rows of the table's Go type and `DefineExists` registers a standalone boolean query. A narrow read needs grants on its referenced columns. A conditional update can write a column on which the role has only `UPDATE`. [Mapped table example](docs/COOKBOOK.md#mapped-table-reads) and [minimum grants](docs/AUTHORIZATION.md#physical-sources-for-typed-plans)

Use `DefineTableRead` to apply explicit SELECT clauses to a full or partial mapped record. The source supplies the projection and its decoder.

Set `RowsSpec.Lock` on a filtered or ordered page, or enable `WithExists` on an unlocked plan for a boolean result. Registered transaction advisory locks and PostgreSQL functions also use `plan.Bind(session)`. Each statement identifies its result, session requirements and minimum grants. [Lock and routine examples](docs/COOKBOOK.md#registered-locks-and-functions)

Use `DefineRead` for inner, left and cross joins. Give each side of a self-join its own occurrence. Use `In` for a fixed list, `Any` for an array parameter and `Like` or `ILike` for patterns. [Explicit join example](docs/COOKBOOK.md#explicit-joins) Add `WithList(ReadListSpec{...})` for finite joined pages and opt-in exact totals. `CursorFrom` uses the decoded projected order values. [Joined lists](docs/COOKBOOK.md#finite-lists-on-explicit-reads)

Compose nested reads with exported fields, explicit `Outer` references, derived and lateral sources, set operations, and read or recursive common tables. Array expansion and array subqueries preserve SQL NULL semantics. Only the final plan registers an executable statement. [Query composition](docs/QUERY-COMPOSITION.md)

Window functions keep their input rows and expose explicit partitions, orders and frames. Pagination operates on the final result, retaining its key and comparison premises. Multiple row-lock clauses give each physical occurrence its own strength and wait option. [Windows and locks](docs/WINDOWS.md)

`InsertInto`, `UpdateOf` and `DeleteFrom` declare typed writes with defaults, conflicts, matching sources and returned rows. Register count results with `DefineWrite` or row results with `DefineReturning`; batch and returning operations use a guarded operation savepoint. Verify write results with the non-executing `pgxdriver.SQLVerifier` backend, including when execution uses `stdsql`. [Typed writes](docs/TYPED-WRITES.md)

## Install v0.4.4

Run `go get github.com/0xJean/tabula@v0.4.4` in your application module with Go 1.26 or later. For the native driver and telemetry modules, use the matching version with `go get github.com/0xJean/tabula/pgxdriver@v0.4.4` and/or `go get github.com/0xJean/tabula/oteltabula@v0.4.4`.

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

A descriptor can match a role that only updates selected columns or only appends rows. Set `TableSpec.WriteMode` to derive the corresponding SQL and column grants. [Table write modes](docs/AUTHORIZATION.md#table-write-modes)

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

## Working from the v0.3 checkout

Run `scripts/workspace.sh` from the Tabula repository root to use all local modules together. In an application repository, check the selected module with `go list -m -json github.com/0xJean/tabula`. [Version selection and adapter setup](docs/INTEGRATION.md)

## What it will not do

- Joins, preloading or lazy loading generated from struct relations.
- AutoMigrate or any DDL generation. Migrations stay the source of truth.
- Global registries, implicit hooks or callbacks. Everything is passed in explicitly.

## Layout

| Package | Role | Dependencies |
| --- | --- | --- |
| `tabula` | Engine: registry, tables, views, composed queries, aggregates, embedded SQL files, sealed sessions, capabilities, errors, logging, tracing/metrics interfaces | standard library only |
| `tabula/pgtype` | PostgreSQL catalog types and input encoders for typed fields and parameters | standard library only |
| `tabula/stdsql` | Role-bound `*sql.DB` / `*sql.Tx` `Runner`, `Shared`, `RawSession`, `ScopePolicy`, `Savepoint`, read-only `Direct` pool | standard library only |
| `tabula/sqlcheck` | Descriptor and catalog snapshots, source SQL & security call-site inventory, role workloads, grant minimization, login-role hygiene and RLS checks | standard library only |
| `tabula/pqdriver` | `lib/pq` array converters | `github.com/lib/pq` |
| `tabula/pgxdriver` | Separate module: role-bound `pgx/v5` pool `Runner`, `Shared`, `RawSession`, `Session`, `Direct`, binary COPY and array converters | `github.com/jackc/pgx/v5` |
| `tabula/oteltabula` | Separate module: OpenTelemetry `Tracer` and `Metrics` adapters | `go.opentelemetry.io/otel` |
| `tabula/postgresfixture` | Content-addressed templates, migration prefixes, isolated test databases and idle template cleanup | standard library only |
| `tabula/cmd/tabula` | SQL verification (`tabula check`), RLS audit (`tabula check-rls`), login role audit (`tabula check-login-role`) and role workload tools. [All commands and flags](docs/REFERENCE.md#cli) | `sqlcheck`, `github.com/lib/pq` |

Applications may use `stdsql` or `pgxdriver` directly, or wrap `tabula` in a thin
binding package of their own to enforce compile-time session/scope invariants.
Both runners establish read-only transactions before the callback. Select the
isolation level in the transaction options. For unscoped reads alongside scoped writes in
one transaction, configure a dedicated reader with `UnscopedReadRole`. [Runner migration notes](docs/MIGRATION.md#from-v030-to-v032)

For startup checks, `sqlcheck.CheckReadiness` verifies the minimum effective grants and RLS flags of a runner role in one catalog query. [Role readiness example](docs/AUTHORIZATION.md#check-a-role-at-startup)

Disposable test suites can label each process login, bind test databases to its lease and reclaim interrupted workers’ roles and idle clones through `postgresfixture`. When migrations create the login's member roles, call `Fixture.Prepare(ctx, plan)` on the empty cluster, acquire the login with `AcquireLoginRole` and its `MemberOf` list, then call `CloneWithLease` for each test. [Process login roles](docs/AUTHORIZATION.md#process-login-roles)

## Contributing

See the [contributing guide](CONTRIBUTING.md) for local checks and the [code of conduct](CODE_OF_CONDUCT.md) for community expectations and moderation.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

The [M3 design](docs/DESIGN-M3.md) records the query, write-effect and verification contracts. Check the changelog for this checkout's release status.
