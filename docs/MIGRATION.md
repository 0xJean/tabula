# Migrating to Tabula v0.3

Follow this guide when upgrading an application to v0.3. For the full surface after the upgrade, see the [integration guide](INTEGRATION.md).

## From v0.3.0 to v0.3.1

v0.3.1 moves role pinning, timeouts, managed scope variables, session sealing, raw-tx opt-in, session-level observability, aggregates and composed view queries into Tabula. Four breaking or behavioral changes apply to `stdsql` and `pgxdriver` callers:

| v0.3.0 | v0.3.1 | Notes |
| --- | --- | --- |
| `stdsql.NewRunner(db, policy)` | `stdsql.NewRunner(db, policy, stdsql.RunnerOptions{...})` returning `(*Runner[S], error)` | Validates role requirements, timeout bounds, `Unscoped` mode and `ManagedVars` at construction. |
| `pgxdriver.NewRunner(pool, policy)` | `pgxdriver.NewRunner(pool, policy, pgxdriver.RunnerOptions{...})` returning `(*Runner[S], error)` | Same validation and role/timeout/GUC guarantees as `stdsql`. |
| `ScopePolicy.SetupTx` | `ScopePolicy.ManagedVars` + `ScopeVars` + `ContextVars` + `UnscopedVars` + `RunnerOptions.Role` + `RunnerOptions.Timeouts` | Runner issues `SET LOCAL ROLE` and one batched `pg_catalog.set_config(..., true)` and auto-guards managed variables and timeout settings. `Direct` rejects policies declaring managed variables. |
| `sess.Tx()` | `RunnerOptions{ExposeTx: true}` + `runner.WithinRaw` / `WithinRawOptions` / `WithinRawTxOptions` → `rawSess.Tx()` | Default `*Session[S]` no longer exposes `*sql.Tx` / `pgx.Tx`. Raw escape sites are opt-in and reported by `tabula inventory` (`AuditSecurityCalls: true`). |
| Unscoped sessions permitted by default on `Runner` | `RunnerOptions.Unscoped` defaults to `UnscopedDenied` | Explicitly choose `UnscopedReadOnly` or `UnscopedReadWrite` on runners that serve unscoped operations. |
| Nested runner calls allowed | Nested `Within*` using a transaction context returns `tabula.ErrNested` before `BEGIN` | Use `Savepoint` for nested work, or `tabula.WithoutTransaction(ctx)` only for deliberate independent transactions (beware connection pool deadlock). |
| Retained session handles stayed callable | Handles close automatically when the callback returns (including errors/panics), returning `tabula.ErrSessionClosed` | Use `RunOptions.BeforeCommit` for pre-commit work on a fresh handle. |
| Registry hooks only | `RunnerOptions.Tracer` / `Metrics` / `Log` / `TagStatements` take precedence per signal over `RegistryConfig` | Each enabled signal has one owner so descriptor statements are never double-emitted; hand-written inline SQL is also observed. |

To adopt **sealed sessions (A4)** incrementally:
1. Construct package-level registries with `RequireSealed: true`.
2. During startup, allocate `key := tabula.NewSessionKey()`, pass `SessionKey: key` into every `RunnerOptions` (and `stdsql.DirectSessionKey(key)` / `pgxdriver.DirectWithConfig(..., DirectConfig{SessionKey: key})`), and call `reg.Seal(key)` checking the error.
3. Unsealed handles (including `NewSession` migration wrappers) fail closed with `tabula.ErrForeignSession` on keyed registries.

## From the v0.2.0 release

| v0.2.0 declaration or call | v0.3 equivalent |
| --- | --- |
| `Define(reg, Table[Record, int64, Scope]{…})` | `Define(reg, TableSpec[Record, int64]{…})` |
| `DefineView(reg, View[Summary, Scope]{…})` | `DefineView(reg, ViewSpec[Summary]{…})` |
| `` `db:"tenant_id,tenant"` `` | `` `db:"tenant_id,scope"` `` |
| `RegistryConfig{TenantID: …, TenantWrap: …}` | `RegistryConfig{ScopeID: …, ScopeWrap: …}` |
| `sess.Tenant()`, `sess.Platform()` | `sess.Scope()`, `sess.Unscoped()` |
| `tbl.Name` | `tbl.Name()` |
| `rows.Get(ctx, scope, id)`, `rows.Lock(ctx, scope, id)` | `rows.Get(ctx, id)`, `rows.Lock(ctx, id)` |
| `rows.List(ctx, scope, where, page)` | `rows.List(ctx, where, page)` |
| `view.One(ctx, scope, sql, args…)` | `view.One(ctx, sql, args…)` |
| `session.Exec(ctx, scope, sql, args…)` | `session.Exec(ctx, sql, args…)` |
| `Listing{Lock: true}` | `Listing{Lock: LockUpdate}` |
| `pqdriver.SQLState`, `pqdriver.IsUniqueViolation` | `tabula.SQLState`, `tabula.IsUniqueViolation`. Other SQLSTATE predicates also live in the root package. |

v0.3 adds the `stdsql` and `pgxdriver` session adapters, driver-independent `tabula.Retry`, `KeysetListing`, `TableSpec.ReadOnly` and opt-in physical delete through `TableSpec.AllowDelete`. The [changelog](../CHANGELOG.md) lists additions separately from changes to existing calls.

Declare the name, default order, escape columns, soft-delete policy and write permissions in the spec. The registry supplies the scope type. The compiled table or view keeps its own bindings. `Definition()` returns a snapshot. `Statement`, `Requires`, `RequiresFunction` and `DefineUpsert` register additional SQL and privilege metadata during application initialization.

### Move row-scope selection to the transaction boundary

Open a runner with the authenticated scope (for example a tenant, organization, or workspace ID). Derived reads and deletes use `Session.Scope()`. Inserts, updates, upserts and pgx COPY check the row's `,scope` column against that bound scope before sending SQL. For structured scopes, `RegistryConfig.ScopeID` should return a stable SQL identity. A mismatch returns `ErrScopeMismatch`. An unscoped session (`Session.Unscoped() == true`) can address rows across scopes.

For a scope-visible pagination total, use `CountExact` (the default). `CountEstimate` reads PostgreSQL's table-wide row estimate and includes all rows across scopes. `CountNone` skips the count.

If a request supplies a second, independently obtained scope ID, preserve that authorization check in the application binding. For example, when both IDs are comparable strings:

```go
if requestScope != sess.Scope() {
    return ErrScopeAuthorization // application error
}
row, err := notes.Bind(sess).Get(ctx, id)
```

Custom session adapters remove the scope parameter from `Exec`, `Scan`, `Returning` and `Each`, and expose `Scope() S` and `Unscoped() bool`. Keep both stable for the lifetime of a binding. An `Each` implementation must stop on a callback error, close the cursor and return the error. Iterators use this contract to release resources on `break`.

Hand-written SQL keeps its original placeholders and arguments. A query containing `WHERE tenant_id = $1` still needs that argument. PostgreSQL RLS can enforce isolation on joins and other application SQL. The runnable example uses an application role and a tenant-local setting. [Multitenant example](../examples/multitenant/README.md)

## From database/sql, sqlx or GORM

| Existing code | Tabula equivalent | What to move |
| --- | --- | --- |
| `QueryRowContext` and `Scan` by primary key | `table.Bind(sess).Get(ctx, id)` | Add column and key tags. Handle a missing row as a nil pointer with a nil error. |
| `sqlx.GetContext` or `SelectContext` for a join | `view.Bind(sess).One`, `Select` or `Iter` | Keep the query and parameter order. Match SELECT order to the declared fields with `ColsAs` or `SelectAs`. |
| `gorm.Where(...).Order(...).Find(...)` | `rows.Find(ctx, where, listing)` | Put values in filter placeholders and map user sort keys through `OrderSpec`. |
| GORM preloads or association writes | A registered SQL statement and a `ViewSpec` for each result shape | Write the join or the sequence of writes explicitly inside the transaction. |
| `ExecContext` for a conditional update | `SetWhere` or `UpdateWhere` | Check the returned boolean. A false value means the predicate matched no row. |
| An atomic counter or timestamp assignment | `SetExpr(ctx, id, Add("attempts", 1), SetNow("updated_at"))` | Map each target to a declared writable column. Use `Save` for version CAS. |
| SQL migrations or GORM AutoMigrate | Existing SQL migrations plus `sqlcheck` | Apply migrations through your deployment tooling. Prepare the descriptor SQL and check schema, grants and RLS against the migrated database. |

Migrate one query at a time. `stdsql.NewSession` wraps an existing `*sql.Tx` and `pgxdriver.NewSession` wraps a `pgx.Tx`. For an existing transaction, the application performs its scope setup before wrapping it. A `Runner` performs that setup when it owns the transaction.

## Mapping permission errors

Consumers with custom error mappings can use `ClassifyError` to distinguish `ClassRLSDenied` and `ClassInsufficientPrivilege` for SQLSTATE 42501. An RLS filter can also yield a successful SELECT with zero visible rows.

## Adopting the authorization checks

Use ordinary unquoted or double-quoted identifiers in guarded adapter SQL. Active v0.3 session restrictions reject Unicode-escaped identifiers and anonymous `DO` blocks. Keep any administrative `DO` blocks in the application’s migration tooling. The built-in guard also rejects `DISCARD ALL`, which resets role and session state. [Session guard behavior](../SECURITY.md)

Keep existing descriptor verification. Register handwritten adapter methods with `CollectSQL` and assign their statements to roles in `RoleStatements`. Capture the installed catalog, review the expected direct grants, then run `CheckGrantManifest` for a read-only drift report. Run `MinimizeGrants` on a disposable installation before preparing application-owned grant changes. [Configuration and examples](AUTHORIZATION.md)
