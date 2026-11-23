# Design principles

pgtable sits between raw `database/sql` and an ORM. It removes the repetitive parts of
PostgreSQL access (column lists, scanning, nullable handling, single-table statements)
and keeps the parts a reviewer must see (joins, locks, conditional writes) as SQL.

## Positioning

1. **Declarative descriptors.** A Go struct with `db` tags describes one table or one row
   shape. Descriptors are defined once at package init and validated strictly: an invalid
   tag panics at start-up, not at the first query.
2. **Explicit SQL.** Anything beyond single-table CRUD is written by hand. pgtable only
   provides the column list and decodes the result.
3. **Verified against a real database.** Every derived or registered statement can be
   prepared and explained against the target schema, and the full SQL is kept in a
   reviewable snapshot.

## Red lines

These hold for every version. A proposal that crosses one needs a design discussion, not a
pull request.

1. **No generated joins.** No relations in struct tags, no preloading, no lazy loading.
   The number of queries and their SQL are always visible in the caller.
2. **No DDL.** No AutoMigrate, no schema generation. The library may only check that code
   and schema agree.
3. **No hidden state or hooks.** No global registry, no implicit callbacks, no
   `time.Now()` inside the library. Clocks, tracers, loggers, scopes and error policies
   are passed in.

## Core rules

- **Identifiers never come from input.** Table, column and sort names come from
  descriptors or whitelists checked at definition time. Values always go through
  placeholders.
- **Writes confirm what they changed.** A write that must touch one row fails unless it
  touched exactly one; insert-only (`once`) columns and optimistic `version` columns are
  guarded by default.
- **Errors are classified, not translated.** The library reports unique, foreign-key,
  check and not-null violations, serialization failures, deadlocks and cancellation as
  distinct errors; the application maps them to its own error model through an
  `ErrorPolicy`.
- **Scope is generic.** The scope type `S` (for example a tenant) is opaque to the engine.
  Tenant predicates, row-level security variables and similar policies are supplied by the
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
- Supported Go versions: the two most recent releases.
- Supported PostgreSQL versions: those still supported upstream (13 and later at the time
  of writing).
