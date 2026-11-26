# Contributing to Tabula

Thank you for your interest in contributing to Tabula!

The code of conduct applies to contributors and maintainers in issues, pull requests and reviews. [Code of conduct](CODE_OF_CONDUCT.md)

## Design Red Lines

Before opening a pull request, please read [docs/DESIGN.md](docs/DESIGN.md). Every change
must preserve Tabula's core boundaries:

1. **Explicit joins** — each join names its kind and right occurrence in declaration order. Inner and left joins supply a typed predicate. Relations in struct tags, automatic join discovery and preloading are prohibited.
2. **No AutoMigrate or DDL generation** — migrations remain the source of truth.
3. **No hidden global state or implicit hooks** — registries, scopes, loggers, tracers,
   and error policies are passed explicitly.
4. **Zero third-party dependencies in core packages** — `tabula`, `tabula/pgtype`, `tabula/stdsql`,
   `tabula/sqlcheck`, `tabula/postgresfixture`, and `tabula/internal/*` depend only on
   the Go standard library. Driver-specific code belongs in `pqdriver` or the separate
   `pgxdriver` module.

## Publication policy

Use general PostgreSQL examples throughout the project. The mandatory vocabulary and scope rules are in [AGENTS.md](AGENTS.md#1-brand-and-domain-neutrality-is-mandatory). Maintainers review issue text, proposed branches, pull requests and release notes against the same rules.

Install the local checks once per clone, then refresh them when the scanner or hooks change. This requires Git and Python 3.9 or newer. Place the private vocabulary in `~/.config/tabula/denylist` with mode `0600`. Use one literal term or phrase per line. Matching is case-insensitive. `TABULA_NEUTRALITY_FILE` can point to another absolute path outside every linked worktree.

```sh
scripts/install-hooks.sh
python3 -IB scripts/neutrality.py check
```

The installer puts the scanner and its hooks in the clone's common Git directory. Existing and future linked worktrees use the same copy. The pre-commit hook checks staged additions and the branch name. The commit-msg hook checks the proposed message. Pre-push checks outgoing history and destination ref names. The hooks require the local policy even if Git inherited a CI environment.

Before opening or editing a pull request, scan its proposed title and description. The full local check also examines the working tree and outgoing commits. A reported location identifies the file and line. Exit status 0 means clean, 1 indicates a finding and 2 indicates a missing policy or invalid input.

```sh
python3 -IB scripts/neutrality.py check
python3 -IB scripts/neutrality.py text pr-title "$title_file"
python3 -IB scripts/neutrality.py text pr-body "$body_file"
```

The existing CI quality job runs the structural checks using the complete Git history. Added Markdown and Go source must use characters outside the CJK ranges, with a source exception for `docs/brand/`. Commit references must resolve to commits in this repository. Local checks also apply the private vocabulary. CI runs with `CI=true` and requires no vocabulary or repository secret.

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

Release tooling requires Git, Go, jq, Python and the authenticated GitHub CLI. The benchmark module is a development harness. The core, pgxdriver and oteltabula receive matching version tags.

1. Run `scripts/release.sh plan vX.Y.Z` to print the intended operations. Finalize and commit the core sources, integration guide, reference and changelog before preparing the child manifests. The guide title identifies the release series (for example `v0.3`).
2. Run `scripts/release.sh prepare vX.Y.Z`. In a temporary Git clone and module cache, Go builds the candidate core archive from HEAD. With `GOWORK=off`, the script removes core replacements, pins the candidate and runs `go mod tidy` for each child. Review and commit the prepared child manifests and checksums.
3. Run `scripts/release.sh verify vX.Y.Z` against the release PR. Verification scans the changelog section, the proposed release body, and the PR at the candidate revision. It checks the reference, pins, checksums and standalone builds. Before the PR exists, use `--pr-file FILE` with its proposed title and body. If the core changes, rerun preparation. For an offline rehearsal of module resolution alone, use `scripts/release.sh verify-modules vX.Y.Z`.
4. After the PR merges, generate its default release body with `scripts/release.sh notes vX.Y.Z > "$notes_file"`. Use `scripts/release.sh publish vX.Y.Z --notes-file "$notes_file"` on clean `main`. Publication verifies the actual merged PR and the supplied notes, then pushes the root, pgxdriver and oteltabula tags with the main revision in one atomic operation. Create the GitHub Release with `gh release create vX.Y.Z --verify-tag --title vX.Y.Z --notes-file "$notes_file"`. A custom release body uses the same `--notes-file` at verification, publication and release creation.
