# Security Policy

## Reporting a Vulnerability

Report SQL injection in `Filter` or `OrderSpec`, session guard bypasses, sensitive
value leakage in `LogConfig` redaction, and other security findings privately.
Send the following details to the maintainers. [Report a vulnerability](https://github.com/0xJean/tabula/security/advisories/new)

- A clear description of the issue and affected package(s).
- Minimal reproduction steps or a failing Go test.
- Any suggested remediation if known.

## Registered statements

`DenyLegacySQL` rejects SQL text and raw transaction entry on a runner carrying the registry's credential. A statement must originate from a registered shape and match the driver method and receiving session. Public observation metadata and SQL snapshots grant no execution authority. Configure PostgreSQL privileges and row policies for the same role. [Registered execution](docs/AUTHORIZATION.md#registered-execution)

## Security Design Guarantees

- Registered physical row locks require writable transactions. Advisory locks require explicit transactions. Direct rejects both lock effects and function calls declared writable. Read declarations require a reviewed function body and a matching catalog contract. The executing role, server transaction mode and session guards apply to the call. [Role verification](docs/AUTHORIZATION.md#check-a-role-at-startup)

- **Identifiers never come from untrusted input**: table, column, and sort identifiers are
  validated at definition time (`Define` / `MustOrderSpec` / `View.DefineQuery`). Dynamic
  values always bind via numbered PostgreSQL placeholders (`$1..$n`). Composed query
  fragments are lexically checked and fuzzed (`FuzzCompileQueryFragment`), and C3 registers
  all deterministic variants for PostgreSQL `PREPARE`/`EXPLAIN` verification.
- Role workload verification matches registered statements to their owning descriptor
  and query. Missing registrations and unreviewed skips fail the inventory check.
  [Authorization guide](docs/AUTHORIZATION.md)
- **Session provenance and capability limits**: registries configured with `SessionKey` or
  `RequireSealed` + `Registry.Seal(key)` reject foreign, copied, wrapped or unsealed sessions
  with `ErrForeignSession` before I/O. `RegistryConfig.Deny` enforces `DenyDelete`,
  `DenyUpsert`, `DenyUnscopedWrites` and `DenyCopy` across derived writes and `CopyFrom`.
- Explicit table write modes check each bound operation before driver I/O, including empty batches and pgx COPY. Hand-written SQL on a restricted Rows handle returns `ErrOperationDenied`. Configure the corresponding PostgreSQL role to enforce access across every session handle. [Table write modes](docs/AUTHORIZATION.md#table-write-modes)
- **Role-bound runners and no default raw `Tx()` escape**: `stdsql.Runner` and
  `pgxdriver.Runner` bind the configured PostgreSQL roles and timeout policy at construction, validate
  declared `ManagedVars` and auto-guard them, reject nested runner calls (`ErrNested`), close
  handles on callback return (`ErrSessionClosed`), and require explicit `ExposeTx: true` for
  `WithinRaw` / `RawSession.Tx()`. `sqlcheck.CheckLoginRole` and `tabula check-login-role`
  audit login-role hygiene (`LOGIN NOINHERIT`, memberships, direct grants, ownership, `CREATE`).
- In a read-write shared transaction, opt-in `UnscopedReadRole` selects a dedicated reader role before unscoped queries. Its PostgreSQL privileges and RLS policies enforce access inside queries and functions. Provision this role with read grants and reviewed function privileges. The handle accepts reads through `Scan` and `Each` and rejects derived writes, `Exec`, `Returning` and pgx COPY. [Reader role setup](docs/AUTHORIZATION.md#using-a-reader-role-in-a-shared-transaction)
- For application-owned UTF-8 SQL, both runners can check statement boundaries before driver I/O with `SingleStatement`. Use positional bind values. The guard accepts one data command with an optional trailing semicolon and rejects ambiguous ordinary-string quote escapes. Use explicit PostgreSQL escape strings or bind the value. `Statements` also checks the outer command, data-modifying CTEs on read methods, and the outer `RETURNING` clause when required. Method policies reject unsupported CTE syntax such as recursive `SEARCH` and `CYCLE` clauses. The database role, RLS and function privileges govern what an accepted query can do. Native pgx runners with these options reject query rewriters in bind arguments. Driver hooks and explicit access to the raw transaction are trusted application code.
- Both runners set `transaction_read_only` in their setup query when a transaction requests read-only access. PostgreSQL takes the first snapshot before any callback and enforces read-only mode for the lifetime of that transaction. The guarded session rejects `SET TRANSACTION` and changes to `transaction_read_only` or `default_transaction_read_only`. A `set_config` call requires a literal unprotected key. Direct calls through `RawSession.Tx()` bypass client-side SQL guards and return native driver errors. Applications supply trusted SQL and the runner owns commit and rollback. [PostgreSQL transaction modes](https://www.postgresql.org/docs/17/sql-set-transaction.html)
- **Sensitive data is never logged by default**: `LogConfig.Values` defaults to
  `ValuesNever`. Columns tagged `db:"...,secret"` and statements registered with
  `tabula.Secret()` are redacted as `"[redacted]"` even when `ValuesAllowed` is enabled.
- Database errors can echo input values. By default, both drivers and descriptor registries export the class and SQLSTATE to tracers and statement logs. Enabling `RawErrorDiagnostics` permits a tracer to record driver text. OpenTelemetry then includes it in exception events and span status descriptions. SQLSTATE 22P02 and 22003 can quote the offending argument even with `ValuesNever` or a log sanitizer. Metrics record the class. An application that supplies log text through `Sanitize` owns the content of that field. [Telemetry fields](docs/LOGGING.md)
- Enable `RequireCallbackContext` to bind session calls to the current runner callback and its derived contexts. Registries can independently require a signed session. Explicit raw driver access is trusted application code.
- `tabula.GuardNoSessionEscape` blocks role and session-authorization changes, `RESET ALL`,
  `DISCARD ALL`, transaction-ending commands (`COMMIT`, `END`, `ABORT`, non-savepoint
  `ROLLBACK`, `PREPARE TRANSACTION`), and calls to `set_config`. It rejects Unicode-escaped
  identifiers. The lexer recognizes comments, quoted identifiers, dollar-quoted strings,
  `E'...'` escape strings, and single-quoted literals. `tabula.GuardReadOnly` applies the
  same session checks to a single `SELECT` or `WITH` statement. Both Direct adapters also set
  `default_transaction_read_only=on` on the PostgreSQL connection.

- The built-in session guard and any configured `NewSessionGuard` with an active restriction reject anonymous `DO` blocks, including after a statement separator. PostgreSQL executes the body as procedural code, so the guard rejects the whole command. Ordinary upserts can use `ON CONFLICT … DO NOTHING` and `DO UPDATE`. [PostgreSQL DO](https://www.postgresql.org/docs/17/sql-do.html)
- The built-in session guard and any configured `NewSessionGuard` with an active restriction reject `UPDATE pg_settings` (optionally `ONLY` and `pg_catalog`-qualified, including inside a CTE), because PostgreSQL treats an update to an existing row as `SET` on that parameter, subject to the database role’s privileges. Reading `pg_settings` stays allowed unless `ForbidVariableReads` is set. [PostgreSQL pg_settings](https://www.postgresql.org/docs/17/view-pg-settings.html)
- `SessionGuardConfig.ForbidVariableReads` (requires `VariablePrefixes`) also rejects reading the protected namespaces: `current_setting` with a protected or non-literal key, `SHOW` of a protected name or `SHOW ALL`, and access to the `pg_settings` view. A query may use `AS pg_settings` as a result label. Ambiguous uses of the catalog name are rejected. A literal setting name outside the protected prefixes, such as `current_setting('TimeZone')`, remains readable.
- `NewSessionGuard` can protect application-owned GUC prefixes and restrict role, session authorization and `set_config` changes in adapter SQL. Prefixes such as `session.` and `local.` match the qualified parameter name with or without a `SET LOCAL` or `SET SESSION` modifier. Run guarded SQL with PostgreSQL’s standard-conforming strings enabled. The database role remains the enforcement boundary for functions and other indirect operations.
- Disposable fixture logins have random in-memory passwords. PostgreSQL receives a salted SCRAM verifier. The lease is a dedicated maintenance session. Keep credentials out of test output, use trusted member roles, and scope database CONNECT grants to each test database. [Process login roles](docs/AUTHORIZATION.md#process-login-roles)
- Fixture login labels are visible in the PostgreSQL catalog. Choose a public identifier for the label. Automatic clone cleanup checks the catalog ownership mark, the original maintenance lease and connections from every role. Reserve the maintenance database and ownership comments for trusted administrators. [Leased clones](docs/DESIGN-v0.3.9.md)
- Grant manifests audit direct ACLs. Run trial revocations and RLS behavior probes on isolated test databases. Sequences and external function effects can outlive a transaction rollback. [Authorization verification model](docs/AUTHORIZATION.md)
