# Design principles

Tabula sits between raw `database/sql` and an ORM. It removes the repetitive parts of
PostgreSQL access (column lists, scanning, nullable handling, single-table statements)
and keeps the SQL for joins, locks and conditional writes available for review.

## Positioning

1. **Declarative descriptors.** A Go struct with `db` tags describes one table or one row
   shape. Descriptors are defined once at package init and validated strictly: an invalid
   tag panics at start-up, not at the first query.
2. **Explicit SQL.** Single-table plans declare their projections, predicates, locks and
   permitted writes. Each shape has a stable key and reviewable SQL. For a joined query
   in v0.4.0, the caller supplies the SQL and a view decodes its result.
3. **Verified against a real database.** Every derived or registered statement can be
   prepared and explained against the target schema, and the full SQL is kept in a
   reviewable snapshot.

The authorization tooling accepts application-owned role assignments, policy categories and expected ACLs. It reads PostgreSQL catalogs, trials revocations in a rolled-back transaction and checks real row behavior in isolated fixtures. [Authorization design](AUTHORIZATION.md)

## Red lines

These hold for every version. A proposal that crosses one needs a design discussion, not a
pull request.

1. **No generated joins.** No relations in struct tags, no preloading, no lazy loading.
   The number of queries and their SQL are always visible in the caller.
2. **No DDL.** No AutoMigrate, no schema generation. The library may only check that code
   and schema agree.
3. **No hidden state or hooks.** The application supplies the registry, scope and error
   policy and owns lifecycle decisions. Opt-in logging and metrics use `LogConfig.Now`
   for statement duration, with `time.Now` as the default.

## Core rules

- **Identifiers never come from input.** Table, column and sort names come from
  descriptors or whitelists checked at definition time. Values always go through
  placeholders.
- **Writes confirm what they changed.** A write that must touch one row fails unless it
  touched exactly one; insert-only (`once`) columns and optimistic `version` columns are
  guarded by default.
- **Errors retain their database meaning.** `ClassifyError` exposes the PostgreSQL
  error class and sentinel. Applications map them to their own errors through an
  `ErrorPolicy`. The [core reference](REFERENCE.md#tabula) lists the classes.
- **Scope is generic.** The scope type `S` (for example a tenant, workspace, or org ID) is opaque to the engine.
  Row-scope predicates, row-level security variables and similar policies are supplied by the
  application binding.
- **The core depends on the standard library only.** Drivers (`lib/pq`, `pgx`),
  adapters and integrations live in separate packages or modules so that users import only
  what they use.
- **Logging is opt-in and explicit.** The logger (`log/slog`), its level switch, the slow
  threshold and the value policy are passed in; logging is off by default and argument
  values are never logged unless the application allows it. See [LOGGING.md](LOGGING.md).
- **Reflection happens once.** Field indexes and codecs are computed at definition time;
  the query path does no tag parsing.

## Application binding

An application owns a small binding package that:

- fixes the session type and makes `Bind` accept only sessions that already enforce the
  application's isolation rules;
- fixes the scope type and how it turns into SQL arguments;
- registers extra value types (UUID, decimal, ...) and the error policy.

The binding is where application-specific rules live. The library stays free of them, so
an application should rarely need to fork it.

## Compatibility

- Semantic versioning. Breaking changes are allowed in `v0.x` with a changelog entry;
  from `v1.0.0` the public API is stable within the major version.
- Support the two most recent Go releases. The development branch tests Go 1.26 and 1.27 with automatic toolchain upgrades disabled.
- The integration matrix runs PostgreSQL 14 through 17.
