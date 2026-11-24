# Contributing to Tabula

Thank you for your interest in contributing to Tabula!

The code of conduct applies to contributors and maintainers in issues, pull requests and reviews. [Code of conduct](CODE_OF_CONDUCT.md)

## Design Red Lines

Before opening a pull request, please read [docs/DESIGN.md](docs/DESIGN.md). Every change
must preserve Tabula's core boundaries:

1. **No generated joins or preloading** — joins stay explicit SQL decoded through `View`.
2. **No AutoMigrate or DDL generation** — migrations remain the source of truth.
3. **No hidden global state or implicit hooks** — registries, scopes, loggers, tracers,
   and error policies are passed explicitly.
4. **Zero third-party dependencies in core packages** — `tabula`, `tabula/stdsql`,
   `tabula/sqlcheck`, `tabula/postgresfixture`, and `tabula/internal/*` depend only on
   the Go standard library. Driver-specific code belongs in `pqdriver` or the separate
   `pgxdriver` module.

## Local Development & Testing

With Go and jq installed, run `scripts/workspace.sh` from the repository root. It creates a Git-ignored `go.work` and maps each child module's core dependency to the local checkout. Run it again after changing the module pins.

```sh
# One-command local gate (static + unit -race + 1s fuzzing)
scripts/check.sh --unit

# Full gate including real PostgreSQL integration tests, coverage floor, and benchmark smoke
export TABULA_TEST_DSN='postgres://tabula:tabula@localhost:5432/tabula?sslmode=disable'
scripts/check.sh --all
```

See [docs/TESTING.md](docs/TESTING.md) for the test tiers, isolation rules, coverage floors, and mandatory integration scenarios, and [.golangci.yml](.golangci.yml) for the repository lint rules. Pull requests should include unit tests and, when touching SQL generation, schema checks, RLS checks, or driver adapters, real PostgreSQL integration tests.

## Development API

The root and pgxdriver modules target Go 1.26 and 1.27. The cross-library dependencies live in the benchmark module. Independently versioned modules use the core's public packages. Keep local replacements in the workspace. CI creates a fresh workspace per job and checks standalone builds during its release rehearsal.

CI runs the race detector and integration suites on PostgreSQL 14–17, publishes coverage profiles, and runs golangci-lint, short fuzz sessions and govulncheck. To reproduce the quality checks with the CI tools installed:

```sh
for dir in . pgxdriver benchmarks; do
  (cd "$dir" && golangci-lint run --build-tags=integration && govulncheck -test -tags=integration ./...)
done
for target in FuzzParseColumnTag FuzzFilterAnd FuzzOrderSpecResolve FuzzGuardNoSessionEscape; do
  go test -run='^$' -fuzz="^${target}$" -fuzztime=1s .
done
go test -run='^$' -fuzz='^FuzzParseSnapshot$' -fuzztime=1s ./sqlcheck
```

## Keep each version's docs with its code

Edit documentation in the same change as the behavior it describes. A release tag captures the code, guides, reference and examples together. Link between files with relative paths so readers stay on that revision.

- The [integration guide](docs/INTEGRATION.md) maps application tasks to packages and commands. Update it when a capability is added, renamed or removed.
- Go declarations, GoDoc and CLI flag definitions own signatures and defaults. Run `scripts/docs.sh --write` after changing them and review [the generated reference](docs/REFERENCE.md). The static gate regenerates it and checks that every public package and command has an entry in the guide.
- Keep examples and behavioral details in the relevant guide. The [cookbook](docs/COOKBOOK.md) covers runtime use, [authorization](docs/AUTHORIZATION.md) covers workload and policy configurations, and [logging](docs/LOGGING.md) covers telemetry. Link to these from other pages. Update an executable example or behavioral test when a contract changes.
- Put upgrade instructions in the [migration guide](docs/MIGRATION.md), history in the [changelog](CHANGELOG.md) and proposed features in the [roadmap](ROADMAP.md). Check the old tag when describing a previous signature.

## Releasing the modules

Release tooling requires Git, Go and jq. The benchmark module is a development harness. The core, pgxdriver and oteltabula receive matching version tags.

1. Run `scripts/release.sh plan vX.Y.Z` to print the intended operations. Finalize and commit the core sources, integration guide, reference and changelog before preparing the child manifests. The guide title identifies the release series (for example `v0.3`).
2. Run `scripts/release.sh prepare vX.Y.Z`. In a temporary Git clone and module cache, Go builds the candidate core archive from HEAD. With `GOWORK=off`, the script removes core replacements, pins the candidate and runs `go mod tidy` for each child. Review and commit the prepared child manifests and checksums.
3. Run `scripts/release.sh verify vX.Y.Z` to check the guide series, generated reference, pins, checksums and standalone builds against the candidate. If the core changes, rerun preparation. Go excludes nested modules from the core archive, so committing the prepared child manifests preserves its checksum.
4. After the release PR merges, run `scripts/release.sh publish vX.Y.Z` on clean `main` at the merged commit. It repeats verification and sends that commit to `main`, `vX.Y.Z`, `pgxdriver/vX.Y.Z` and `oteltabula/vX.Y.Z` in one atomic push. CI only exercises preparation and verification.
