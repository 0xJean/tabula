# Tabula Agent & Contributor Constraints

This file defines the hard engineering constraints for contributors and coding agents working in `github.com/0xJean/tabula`. Read [docs/DESIGN.md](docs/DESIGN.md) and [docs/TESTING.md](docs/TESTING.md) before making architectural or API changes.

## 1. Identity & Red Lines

Tabula is a **general-purpose, domain-agnostic PostgreSQL descriptor engine** for Go.

1. **No generated joins or preloading** — relations never go into struct tags; joins remain explicit SQL decoded via `View`.
2. **No DDL or AutoMigrate** — migrations remain the sole source of truth; Tabula may only verify schema/grants/RLS via `sqlcheck`.
3. **No hidden global state, callbacks, or implicit clocks** — registries, scopes, loggers, tracers, metrics, and error policies are passed explicitly.
4. **Strict domain neutrality** — never introduce business-specific concepts, terminology, roles, GUC names, or test fixtures from any host/downstream application (e.g., finance, crypto, payments, KYC, auth tokens). Keep row-level scoping generic (`Scope` / `Unscoped` / `db:"...,scope"`). Multi-tenant PostgreSQL RLS may appear in examples, cookbooks, and tests as an illustration of generic scoping.
5. **Zero third-party dependencies in core packages** — `tabula`, `tabula/stdsql`, `tabula/sqlcheck`, `tabula/postgresfixture`, and `tabula/internal/*` must depend **only on the Go standard library**. Driver code belongs in `pqdriver` (`lib/pq`) or the separate `pgxdriver` module (`pgx/v5`).

## 2. Repository & Module Boundaries

- Root package (`package tabula`) stays at the repository root so callers import `github.com/0xJean/tabula`. Do not relocate core code under `pkg/` or `core/`.
- Keep files cohesive and readable (avoid monolithic files; prefer focused files around 100–500 lines).
- **Three Go modules** live in this repo: root (`.`), `pgxdriver/`, and `benchmarks/`.
  - Never commit `replace` directives to any `go.mod`. Use `scripts/workspace.sh` to generate a git-ignored `go.work` for local multi-module work.
  - Submodules (`pgxdriver`, `benchmarks`) must import **only public packages** of root `tabula` — never `github.com/0xJean/tabula/internal/...`.
- Supported targets: the two most recent Go releases (`go 1.26.0` directive; CI tests Go 1.26.x & 1.27.x with `GOTOOLCHAIN=local`) and PostgreSQL 14–17.

## 3. SQL Safety & Security Invariants

- Identifiers (table, column, sort expressions) come only from descriptors or init-time whitelists (`Define`, `MustOrderSpec`); runtime values always bind via `$1..$n`.
- Changes to the [SQL lexer](internal/pgstmt/lex.go), [built-in guards](guard.go), or [shared session guard](session_guard.go) must include both bypass-regression tests (e.g. comments, quoted strings, dollar quotes, `E'...'` escape strings, stacked statements) and fuzz seeds.
- Read-only enforcement (`TableSpec.ReadOnly`, `WithinReadOnly`, `Direct`) must fail closed with `tabula.ErrReadOnly` and preserve SQLSTATE `25006` when rejected by PostgreSQL.
- `Direct` pool options must never weaken `default_transaction_read_only=on`.

## 4. Testing, Lint & Verification Gates

- Follow [docs/TESTING.md](docs/TESTING.md):
  - Unit tests (`go test -race`) must not require a database.
  - Any change touching SQL generation, codecs, drivers, transactions, `Direct`, `CopyFrom`, `sqlcheck`, or `cmd/tabula` **must** be verified against a real disposable PostgreSQL instance (`-tags=integration -race`).
  - Integration tests must **fail** (never silently skip) when `TABULA_TEST_DSN` is unset. Never connect to or mutate non-disposable application databases.
  - Use standard `testing` + `github.com/stretchr/testify/require` (no `mock` or `suite` packages).
- Before every commit, run the shared gate:
  - Without PG: `scripts/check.sh --unit`
  - With disposable PG (`TABULA_TEST_DSN` set): `scripts/check.sh --all`
- Lint rules live in [.golangci.yml](.golangci.yml) (golangci-lint v2). Keep code compliant with all enabled linters (`standard` + `errorlint`, `misspell`, `revive`, `rowserrcheck`, `sqlclosecheck`, `unconvert`).

## 5. Documentation & Releases

- Any public API, tag option, error sentinel, CLI flag, or behavior change must update GoDoc comments plus the affected docs ([README.md](README.md), [CHANGELOG.md](CHANGELOG.md), [docs/COOKBOOK.md](docs/COOKBOOK.md), [docs/MIGRATION.md](docs/MIGRATION.md), [docs/LOGGING.md](docs/LOGGING.md), [SECURITY.md](SECURITY.md)) in the same commit.
- Never push to `origin` or create/push release tags unless explicitly asked by the user.
- Releases must go through [scripts/release.sh](scripts/release.sh) (`plan` → `prepare` → commit manifests → `verify` → atomic `publish` of `main`, `vX.Y.Z`, and `pgxdriver/vX.Y.Z`).
