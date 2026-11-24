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
  validated at definition time (`Define` / `MustOrderSpec`). Dynamic values always bind
  via numbered PostgreSQL placeholders (`$1..$n`).
- **Sensitive data is never logged by default**: `LogConfig.Values` defaults to
  `ValuesNever`. Columns tagged `db:"...,secret"` and statements registered with
  `tabula.Secret()` are redacted as `"[redacted]"` even when `ValuesAllowed` is enabled.
- `tabula.GuardNoSessionEscape` blocks role and session-authorization changes, `RESET ALL`,
  `DISCARD ALL`, and calls to `set_config`. It rejects Unicode-escaped identifiers.
  The lexer recognizes comments, quoted identifiers, dollar-quoted strings, `E'...'`
  escape strings, and single-quoted literals. `tabula.GuardReadOnly` applies the same
  session checks to a single `SELECT` or `WITH` statement. Both Direct adapters also set
  `default_transaction_read_only=on` on the PostgreSQL connection.

- `NewSessionGuard` can protect application-owned GUC prefixes and restrict role, session authorization and `set_config` changes in adapter SQL. Run guarded SQL with PostgreSQL’s standard-conforming strings enabled. The database role remains the enforcement boundary for functions and other indirect operations.
- Grant manifests audit direct ACLs. Run trial revocations and RLS behavior probes on isolated test databases. Sequences and external function effects can outlive a transaction rollback. [Authorization verification model](docs/AUTHORIZATION.md)
