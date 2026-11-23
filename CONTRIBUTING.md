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

```sh
# Format and vet
gofmt -l .
go vet -tags=integration ./...
(cd pgxdriver && go vet -tags=integration ./...)

# Unit tests
go test -race -count=1 ./...
(cd pgxdriver && go test -race -count=1 ./...)

# Real PostgreSQL integration tests (use a disposable database)
export TABULA_TEST_DSN='postgres://tabula:tabula@localhost:5432/tabula?sslmode=disable'
go test -race -count=1 -tags=integration ./...
(cd pgxdriver && go test -race -count=1 -tags=integration ./...)
```

Pull requests should include unit tests and, when touching SQL generation, schema checks,
RLS checks, or driver adapters, real PostgreSQL integration tests.
