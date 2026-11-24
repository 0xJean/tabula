<p align="center"><img src="docs/brand/lockup-light.png" alt="Tabula" width="420"></p>

# Tabula

[![CI](https://github.com/0xJean/tabula/actions/workflows/ci.yml/badge.svg)](https://github.com/0xJean/tabula/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/0xJean/tabula.svg)](https://pkg.go.dev/github.com/0xJean/tabula) [![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

Declarative descriptors, explicit SQL, verified against a real PostgreSQL.

Tabula describes PostgreSQL tables and result rows with Go struct tags, derives the
plain single-table statements from them, decodes the rows of hand-written SQL, and
checks every registered statement against a real database. It is not an ORM: it never
builds joins, never generates DDL, and never runs hidden callbacks.

**Integrating v0.3?** Start with the [application integration guide](docs/INTEGRATION.md). It maps each task to its package or command. The [complete Go API and CLI reference](docs/REFERENCE.md) is generated from this checkout.

Read the README at the Git tag or commit your application builds. The [migration guide](docs/MIGRATION.md) covers upgrading to v0.3. The [changelog](CHANGELOG.md) records release status.

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

`Get` returns a pointer to the matching `Item`, or `(nil, nil)` if the key is missing.

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

## Contributing

See the [contributing guide](CONTRIBUTING.md) for local checks and the [code of conduct](CODE_OF_CONDUCT.md) for community expectations and moderation.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
