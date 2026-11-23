# Tabula

Declarative descriptors, explicit SQL, verified against a real PostgreSQL.

Tabula describes PostgreSQL tables and result rows with Go struct tags, derives the
plain single-table statements from them, decodes the rows of hand-written SQL, and
checks every registered statement against a real database. It is not an ORM: it never
builds joins, never generates DDL, and never runs hidden callbacks.

> Status: pre-release. The engine was extracted from a production refactoring project
> (v0.2 there). See [ROADMAP.md](ROADMAP.md) for what comes next and
> [docs/DESIGN.md](docs/DESIGN.md) for the rules the library keeps.

## What it does

| Need | Tabula |
| --- | --- |
| Single-table CRUD | `Define(Table[T, K, S]{Name, Order})` derives get, lock, insert, update, count and list from `db` tags (key, serial, tenant, deleted, version, once, json, zero, array) |
| Rows of hand-written SQL (joins, projections, reports) | `DefineView(View[T, S]{...})`: you write the SQL, Tabula decodes the rows |
| Batch and locked reads | `GetMany`, `LockMany` (fixed key order), `LockBy` |
| Safe dynamic parts | `Filter` (parameters only), `OrderSpec` (whitelisted sort columns) |
| Logging | Opt-in `log/slog` with level switch, slow-statement threshold and value redaction ([spec](docs/LOGGING.md)) |
| Verification | `sqlcheck`: PREPARE and EXPLAIN every registered statement, compare columns with the schema, derive privileges, snapshot the SQL for review |

## What it will not do

- Joins, preloading or lazy loading generated from struct relations.
- AutoMigrate or any DDL generation. Migrations stay the source of truth.
- Global registries, implicit hooks or callbacks. Everything is passed in explicitly.

## Layout

| Package | Role | Dependencies |
| --- | --- | --- |
| `tabula` | Engine: registry, tables, views, rows, filter, order | standard library only |
| `tabula/pqdriver` | lib/pq SQLSTATE helpers and array converters | `github.com/lib/pq` |
| `tabula/sqlcheck` | Snapshot, schema and privilege verification | standard library only |

Applications bind the engine through a thin layer of their own: the session type, the
scope type (for example a tenant), the error policy and extra value types. Tabula has no
notion of tenants or business errors.

## License

To be decided before the first public release.
