# Contributing to Tabula

Thank you for your interest in contributing to Tabula!

## Design Red Lines

Before opening a pull request, please read [docs/DESIGN.md](docs/DESIGN.md). Every change
must preserve Tabula's core boundaries:

1. **No generated joins or preloading** — joins stay explicit SQL decoded through `View`.
2. **No AutoMigrate or DDL generation** — migrations remain the source of truth.
3. **No hidden global state or implicit hooks** — registries, scopes, loggers, tracers,
   and error policies are passed explicitly.
4. **Zero third-party dependencies in core packages** — `tabula`, `tabula/stdsql`, and
   `tabula/sqlcheck` depend only on the Go standard library. Driver-specific code belongs
   in `pqdriver` or the separate `pgxdriver` module.

## Local Development & Testing

From the repository root, run `scripts/workspace.sh` once per checkout. It creates a Git-ignored `go.work` for the core, pgxdriver and benchmarks. The tagged core v0.2.0 requires Go 1.27. A version-specific workspace override lets Go 1.26 load the local core while the child manifests still name that tag.

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

## Releasing the modules

Release tooling requires Git, Go and jq. The benchmark module is a development harness. Only the core and pgxdriver receive version tags.

1. Run `scripts/release.sh plan vX.Y.Z` to print the intended operations. Finalize and commit the core sources and changelog before preparing the child manifests.
2. Run `scripts/release.sh prepare vX.Y.Z`. In a temporary Git clone and module cache, Go builds the candidate core archive from HEAD. With `GOWORK=off`, the script removes core replacements, pins the candidate and runs `go mod tidy` for each child. Review and commit both manifests and their checksums.
3. Run `scripts/release.sh verify vX.Y.Z` to check the pins, checksums and standalone builds against the committed core. If the core changes, rerun preparation. Go excludes nested modules from the core archive, so committing the prepared child manifests preserves its checksum.
4. When ready to publish, `scripts/release.sh publish vX.Y.Z` repeats verification on clean `main`. It rejects replacement directives in all three manifests and sends the verified commit to `main`, `vX.Y.Z` and `pgxdriver/vX.Y.Z` in one atomic push. CI only exercises preparation and verification.
