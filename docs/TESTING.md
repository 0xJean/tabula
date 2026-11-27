# Testing Standards & Verification Gates

Every change in Tabula is verified through layered tests and automated gates shared between local development (`scripts/check.sh`) and GitHub Actions (`.github/workflows/ci.yml`).

## 1. Test Tiers

| Tier | Build Tag | Database | Purpose & Rules |
| --- | --- | --- | --- |
| **Unit** | *(none)* | None | Pure Go logic: tag parsing, SQL derivation, filter/order building, codecs, error classification, logging redaction, SQL guards, snapshot round-trips. Must run fast and pass under `-race`. |
| **GoDoc Examples** | *(none)* | None | Runnable `Example*` tests in `example_test.go` keeping public API snippets verified and rendered on pkg.go.dev. |
| **Fuzz** | *(none)* | None | Parser and security-boundary fuzzers (`FuzzParseColumnTag`, `FuzzFilterAnd`, `FuzzOrderSpecResolve`, `FuzzGuardNoSessionEscape`, `sqlcheck/FuzzParseSnapshot`). Must never panic or violate stated invariants. |
| **Integration** | `//go:build integration` | Disposable PostgreSQL (14–17) | Real PostgreSQL execution for CRUD, locks (`FOR UPDATE`, `SKIP LOCKED`, `NOWAIT`, `FOR SHARE`), upserts, batch chunking, `SetExpr`, `Direct`, `CopyFrom`, arrays/network codecs, `sqlcheck` PREPARE/EXPLAIN, privileges, views/materialized views, RLS, and CLI. |
| **End-to-End Example** | *(none)* | Disposable PostgreSQL | `examples/multitenant` runs its real RLS migration and verifies schema, statements, RLS policy, and cross-tenant isolation. |
| **Benchmarks** | `bench_test.go`, `bench_integration_test.go`, `benchmarks/` | Optional / Disposable PG | Microbenchmarks (in-memory engine overhead), real-PG bulk write benchmarks (`InsertMany` vs `CopyFrom`), and the isolated cross-library `benchmarks/` comparison module. |

## 2. Hard Rules

1. **No silent skips in integration tests**: integration tests require `TABULA_TEST_DSN` and must call `require.NotEmpty(t, dsn)` rather than `t.Skip`. Benchmarks may skip when `TABULA_TEST_DSN` is unset.
2. **Strict schema/role isolation**: every integration test must create a unique temporary schema (or unique table name) and clean it up via `t.Cleanup` (`DROP SCHEMA ... CASCADE` and `DROP ROLE ...`). Never rely on shared mutable state between tests.
3. **Never touch production or shared app databases**: local real-PG testing must target a disposable container (e.g. `docker run -d --rm -e POSTGRES_USER=tabula -e POSTGRES_PASSWORD=tabula -e POSTGRES_DB=tabula -p 127.0.0.1::5432 postgres:15-alpine`).
4. **Standard `testing` + `testify/require`**: use subtests (`t.Run`) and `require`/`assert` assertions. Do not introduce `testify/mock`, `testify/suite`, or SQLite substitutes.
5. **No flaky timing dependencies**: do not use `time.Sleep` to synchronize concurrency; use deterministic clocks (`LogConfig.Now`, `RetryConfig.Sleep`) in unit tests and real PostgreSQL row locks/transactions in integration tests.
6. **Domain-neutral fixtures**: keep table names, column values, and test data generic (e.g. `items`, `notes`, `jobs`, `regions`, `entries`). Do not copy business-specific schemas or jargon from host applications.
7. **Security regressions require tests + fuzz seeds**: every fix to `internal/pgstmt`, `GuardNoSessionEscape`, `GuardReadOnly`, `Filter`, or `OrderSpec` must add explicit blocked & allowed test cases plus fuzz seeds.

## 3. When Integration Tests Are Mandatory

Any PR or commit modifying any of the following **must** pass `scripts/check.sh --all` against a real PostgreSQL instance:

- SQL generation (`table.go`, `rows.go`, `write.go`, `upsert.go`, `query.go`, `expression.go`, `view.go`, `filter.go`, `order.go`)
- Value codecs (`values.go`, `network.go`, `pqdriver/`, `pgxdriver/`)
- Transaction/session/direct adapters (`stdsql/`, `pgxdriver/`, `internal/direct/`)
- Verification tooling (`sqlcheck/`, `cmd/tabula/`)
- SQL guards (`guard.go`, `internal/pgstmt/`)

## 4. Automated Gates & Coverage Floors

Run locally via [scripts/check.sh](../scripts/check.sh):

```sh
scripts/workspace.sh
scripts/check.sh --static   # gofmt, module boundaries, stdlib-only core, go vet, builds and reference drift
scripts/check.sh --unit     # --static + unit tests (-race) + 1s fuzzing
TABULA_TEST_DSN='postgres://...' scripts/check.sh --all  # full gate including PG integration & coverage
```

The full gate also runs the workspace integration package in `testdata/integration`. It checks both drivers with minimum append and update grants, verifies registered plans, exercises serial inserts, and revokes individual column and sequence grants to check denials. It also checks callback ownership and exports real PostgreSQL statement failures through OpenTelemetry and structured logs.

The typed read tests compare projected fields and PostgreSQL parameter types, exercise zero, one and multiple matching rows, and run existence checks under narrow column grants. They reuse a single binding across statements and separate sessions with both drivers.

Composable-query tests cover lexical correlation, sets, recursion, array sources, windows, multiple locks and final-result pages. Typed-write tests verify defaults, actual conflict arbiters, minimum grants, matching sources, batches and returned rows with both drivers. Protocol verification tests describe write results without executing them and distinguish logical domain identity from wire OIDs. Operation tests exercise savepoint recovery, retained handles, interrupted iteration, completion cancellation and failures that must prevent the enclosing transaction from committing. Run all five lint scopes with integration tags so the independent workspace integration package is included.

CI ([.github/workflows/ci.yml](../.github/workflows/ci.yml)) additionally runs:

- **Matrix**: Go `1.26.x` and `1.27.x` × PostgreSQL `14`, `15`, `16`, `17`
- **Coverage floors** (measured under `-tags=integration`): root module `>= 75%`, `pgxdriver` `>= 75%`
- **Lint**: `golangci-lint` (v2) with [.golangci.yml](../.golangci.yml) across `.`, `pgxdriver`, and `benchmarks`
- **Security**: `govulncheck -test -tags=integration ./...` across all three modules
- **Example E2E**: `examples/multitenant` migrated and executed against PostgreSQL 17
- **Release rehearsal**: prepare a synthetic patch of the documented minor version and run `release.sh verify-modules`. The release script checks the standalone module pins with `GOWORK=off` and regenerates the reference against the candidate modules.

## Documentation checks

`scripts/docs.sh --check` runs inside the static gate. It compares the committed reference with `go doc -all` for every public product package and the built CLI's help for every registered command. It compares the integration guide’s package and command links in both directions, catching omitted entries and links to retired ones. Use `scripts/docs.sh --write` to regenerate the reference after a reviewed API change.

`TestAuthorizationGuide` runs the named JSON configurations from [the authorization guide](AUTHORIZATION.md) through the CLI against disposable PostgreSQL, including an expected coverage finding. The [GoDoc examples](../example_test.go) and [multitenant application](../examples/multitenant/README.md) provide executable runtime examples. Behavioral changes also require review of the explanatory text and migration instructions.

## Authorization fixtures

The real-database suite exercises direct table, column, sequence and overloaded function grants, temporary revocation and restoration, command coverage, and role-specific row visibility and denial. The test account needs authority to create disposable roles, schemas and databases. [Authorization verification](AUTHORIZATION.md)

The `postgresfixture` tests perform physical database clones, retry a failed install and check isolation between parallel tests. Cleanup tests cover idle and kept templates, interrupted builds, active copies, overlapping pruners, similar database names and malformed last-use marks. [Fixture integration tests](../postgresfixture/fixture_integration_test.go) and [pruning regression tests](../postgresfixture/prune_integration_test.go)

The template preparation tests acquire a login using a migration-created member role, read installed data from a leased clone and verify cache reuse. They also block two workers on the stage lock and run pruning before allowing publication. [Preparation tests](../postgresfixture/prepare_integration_test.go)

The login lifecycle tests launch independent copies of the test binary on the disposable server. They authenticate each login, release all business connections, keep one process alive and kill another. The suite checks role identity, outstanding database grants and the maintenance database that owns each lease. It also verifies that a live client resets the grace period and an expired role is collected. [Process tests](../postgresfixture/login_integration_test.go)

The leased-clone tests run two processes with the same login label, kill one, then race two collectors. They check a live worker's idle database, an unrelated connection to the orphan, ordinary clones, copied marks and same-name replacements. [Leased-clone tests](../postgresfixture/clone_lease_integration_test.go)
