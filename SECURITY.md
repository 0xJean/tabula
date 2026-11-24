# Security Policy

## Reporting a Vulnerability

Report SQL injection in `Filter` or `OrderSpec`, session guard bypasses, sensitive
value leakage in `LogConfig` redaction, and other security findings privately.
Send the following details to the maintainers. [Report a vulnerability](https://github.com/0xJean/tabula/security/advisories/new)

- A clear description of the issue and affected package(s).
- Minimal reproduction steps or a failing Go test.
- Any suggested remediation if known.

## Security Design Guarantees

- **Identifiers never come from untrusted input**: table, column, and sort identifiers are
  validated at definition time (`Define` / `MustOrderSpec` / `View.DefineQuery`). Dynamic
  values always bind via numbered PostgreSQL placeholders (`$1..$n`). Composed query
  fragments are lexically checked and fuzzed (`FuzzCompileQueryFragment`), and C3 registers
  all deterministic variants for PostgreSQL `PREPARE`/`EXPLAIN` verification.
- **Session provenance and capability limits**: registries configured with `SessionKey` or
  `RequireSealed` + `Registry.Seal(key)` reject foreign, copied, wrapped or unsealed sessions
  with `ErrForeignSession` before I/O. `RegistryConfig.Deny` enforces `DenyDelete`,
  `DenyUpsert`, `DenyUnscopedWrites` and `DenyCopy` across derived writes and `CopyFrom`.
- **Role-bound runners and no default raw `Tx()` escape**: `stdsql.Runner` and
  `pgxdriver.Runner` bind one PostgreSQL role and timeout policy at construction, validate
  declared `ManagedVars` and auto-guard them, reject nested runner calls (`ErrNested`), close
  handles on callback return (`ErrSessionClosed`), and require explicit `ExposeTx: true` for
  `WithinRaw` / `RawSession.Tx()`. `sqlcheck.CheckLoginRole` and `tabula check-login-role`
  audit login-role hygiene (`LOGIN NOINHERIT`, memberships, direct grants, ownership, `CREATE`).
- Both runners set `transaction_read_only` in their transaction-local setup query when a transaction requests read-only access. PostgreSQL takes the first snapshot before any callback and enforces read-only mode for the lifetime of that transaction. The guarded session rejects explicit requests for read-write mode with `ErrReadOnly`. An opt-in raw transaction uses the driver's native errors. User SQL belongs to the application, and the runner owns commit and rollback. [PostgreSQL transaction modes](https://www.postgresql.org/docs/17/sql-set-transaction.html)
- **Sensitive data is never logged by default**: `LogConfig.Values` defaults to
  `ValuesNever`. Columns tagged `db:"...,secret"` and statements registered with
  `tabula.Secret()` are redacted as `"[redacted]"` even when `ValuesAllowed` is enabled.
- `tabula.GuardNoSessionEscape` blocks role and session-authorization changes, `RESET ALL`,
  `DISCARD ALL`, and calls to `set_config`. It rejects Unicode-escaped identifiers.
  The lexer recognizes comments, quoted identifiers, dollar-quoted strings, `E'...'`
  escape strings, and single-quoted literals. `tabula.GuardReadOnly` applies the same
  session checks to a single `SELECT` or `WITH` statement. Both Direct adapters also set
  `default_transaction_read_only=on` on the PostgreSQL connection.

- The built-in session guard and any configured `NewSessionGuard` with an active restriction reject anonymous `DO` blocks, including after a statement separator. PostgreSQL executes the body as procedural code, so the guard rejects the whole command. Ordinary upserts can use `ON CONFLICT … DO NOTHING` and `DO UPDATE`. [PostgreSQL DO](https://www.postgresql.org/docs/17/sql-do.html)
- `NewSessionGuard` can protect application-owned GUC prefixes and restrict role, session authorization and `set_config` changes in adapter SQL. Prefixes such as `session.` and `local.` match the qualified parameter name with or without a `SET LOCAL` or `SET SESSION` modifier. Run guarded SQL with PostgreSQL’s standard-conforming strings enabled. The database role remains the enforcement boundary for functions and other indirect operations.
- Grant manifests audit direct ACLs. Run trial revocations and RLS behavior probes on isolated test databases. Sequences and external function effects can outlive a transaction rollback. [Authorization verification model](docs/AUTHORIZATION.md)
