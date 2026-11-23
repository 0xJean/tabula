# Security Policy

## Reporting a Vulnerability

Please do **not** open a public GitHub issue for security vulnerabilities (such as SQL
injection bypasses in `Filter` / `OrderSpec`, session-escape bypasses in
`GuardNoSessionEscape`, or sensitive value leakage in `LogConfig` redaction).

Instead, report vulnerabilities privately via GitHub Security Advisories on
`0xJean/tabula` with:

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
- **Session-escape & read-only guards**: `tabula.GuardNoSessionEscape` blocks adapter SQL
  from executing `SET ROLE`, `RESET ROLE`, `RESET ALL`, `SESSION AUTHORIZATION`, or
  `set_config(...)` (skipping comments, dollar-quoted strings, `E'...'` escape strings, and
  single-quoted literals). `tabula.GuardReadOnly` admits only a single `SELECT` or `WITH`
  statement and combines with `default_transaction_read_only=on` in `stdsql.Direct` and
  `pgxdriver.Direct` for defense-in-depth read-only pools.
