# Roadmap

Every release is validated against real PostgreSQL (14–17) before it is tagged.
Items keep the red lines in [docs/DESIGN.md](docs/DESIGN.md).

## v0.2 — released

- `Table[T, K, S]` / `Rows`: get, lock, insert, save, set, soft delete, count, list, find,
  exists; `GetMany`, `LockMany` (ordered by key), `LockBy`.
- `View[T, S]`: decode rows of hand-written SQL by `db` tags; `Cols`, `ColsAs`,
  `Statement`, `One`, `Select`, `Scan`.
- `Filter`, `OrderSpec`, JSON / zero / array codecs, privilege derivation.
- `pqdriver`; `sqlcheck`: snapshots, schema columns, privilege derivation and role grants,
  `PrepareStatement` / `VerifyStatements` (PREPARE plus EXPLAIN EXECUTE on a real database).

## v0.3 — released (`v0.3.0`–`v0.3.11`)

- **Immutable descriptors & generic row scope**: `TableSpec` / `ViewSpec`, bound-scope
  sessions (`Scope` / `Unscoped` / `db:"...,scope"`), driver-independent `tabula.Retry` and
  SQLSTATE helpers.
- **Single-table writes & table modes**: `UpdateWhere` / `SetWhere`, controlled upsert
  (`DefineUpsert` / `Upsert` / `UpsertMany`), `SetExpr` (`Add`, `SetNow`), opt-in physical
  deletes (`TableSpec.AllowDelete`), `TableSpec.ReadOnly`, `TableSpec.WriteMode`,
  `InsertReturning` / `SaveReturning`, chunked `InsertMany`.
- **Role-bound runners and guarded sessions**: `stdsql` and separate `pgxdriver` (`pgx/v5`)
  modules with role pinning, per-role timeouts, `ScopePolicy`, `WithinShared`, `WithinRetry`,
  `WithinRaw` (`ExposeTx`), `Direct` pools, `CopyFrom`, sealed sessions (`SessionKey` /
  `Registry.Seal`), capability policy (`Deny`), session-level observability, and separate
  `oteltabula` OpenTelemetry module.
- **PostgreSQL authorization & fixture tooling**: `sqlcheck` static/dynamic SQL inventory,
  security call targets, direct grant manifests, revocation trials (`MinimizeGrants`),
  policy snapshots and classification, RLS behavior matrix (`RunRLSMatrix`), login-role
  hygiene (`CheckLoginRole`), `cmd/tabula` CLI, and `postgresfixture` content-addressed test
  templates with process login leases and idle clone pruning. [Authorization guide](docs/AUTHORIZATION.md)

## v0.4 — released (`v0.4.0`–`v0.4.4`)

- **Registered PostgreSQL statements & `SQLAuthority`**: strict `DenyLegacySQL` execution
  channel, typed predicates, single-table row/mutation/existence plans, row locks,
  transaction advisory locks and declared PostgreSQL functions.
- **Explicit joins, mappings and expressions**: inner/left/cross/self-joins, `DefinePlanSource`,
  `MapTable`, `pgtype`, scalar expressions, computed groups/orders, aggregate filters and
  ordered aggregates, `Distinct` / `DistinctOn`, finite joined lists and catalog-proven
  keyset cursors. [v0.4 design](docs/DESIGN-v0.4.md)
- **Composable query graphs & typed writes**: derived/lateral sources, correlated subqueries,
  set operations, read/recursive CTEs, window functions and frames, multiple row-lock clauses,
  typed `INSERT`/`UPDATE`/`DELETE` with defaults/conflicts/batches/`RETURNING`/modifying CTEs,
  guarded `OperationSession` savepoints and non-executing `pgxdriver.SQLVerifier`. [M3 design](docs/DESIGN-M3.md)

## v0.5 — released (`v0.5.0`–`v0.5.1`)

- **Named-parameter SQL tier (`DefineSQL*`)**: `DefineSQL`, `DefineSQLExec`, `DefineSQLReturning`,
  `DefineSQLList`, `ScopeParam`, Go type families (`TypeFamilyOf`, `RegisterTypeFamily`),
  bidirectional `AllowedTypes` catalog verification with lossless input integer widening (`v0.5.1`),
  and custom `RegistryConfig.SessionGuard`.
- **Table-level grant granularity**: `GranularityTable` across `ReadCatalog`, `CheckGrantManifest`,
  `MinimizeGrants`, `cmd/tabula --granularity=table`, and `ReadinessNeeds.TableSelect/Insert/Update/Delete`.

## v0.6 — released (`v0.6.0`)

- **API surface convergence (`github.com/0xJean/tabula/plan`)**: extract the Typed Plan AST
  and scalar/aggregate/window expression DSL into a dedicated stdlib-only core subpackage
  `tabula/plan`, reducing root `package tabula`'s exported surface by roughly two-thirds while
  preserving `SQLAuthority` unforgeability and zero-global-state guarantees.
- **Documentation & version hygiene**: add the one-page [Choosing an API](docs/CHOOSING-AN-API.md)
  decision guide with a minimal starter binding, declutter `README.md`, and enforce doc/tag
  version consistency in `scripts/docs.sh` and `scripts/release.sh`.

Note: expression-level `COLLATE`, recursive `SEARCH`/`CYCLE` clauses and native record paths
are already usable via `DefineSQL*` and remain optional future AST additions rather than
blockers for `v1.0`.

## v1.0 — stability checklist

- Freeze public APIs and semver guarantees across `tabula`, `tabula/plan`, `tabula/pgtype`,
  `tabula/stdsql`, `tabula/sqlcheck`, `tabula/pgxdriver` and `tabula/oteltabula`.
- Retire or clearly quarantine deprecated `v0.3` string-fragment helpers if needed prior to freeze.
- Optional Prometheus adapter module over `Metrics`.
- Optional prepared-statement cache.

## Out of scope

Relationship inference from struct fields, preloading, AutoMigrate or DDL generation, global hooks or callbacks, and databases other than PostgreSQL.
