# Migrating to Tabula v0.4

Start with the section for the version you are adopting. The integration guide lists the APIs and commands available in this checkout. [Integration guide](INTEGRATION.md)

## From v0.4.0 to v0.4.1

Declare one `FromTable` occurrence per alias. Pass it to `Select(...).From(...)` or a join, or use `RowsFrom` to specify a custom catalog type for a scope or soft-delete column. Declare a nullable view field for a projected column on the right of a left join. Bind the resulting `DefineRead` plan to a session and execute `Find`. Rebuild the role snapshot and run readiness and statement verification for the new shapes. [Explicit joins](COOKBOOK.md#explicit-joins)

Use `In(column, first, rest...)` when the number of operands is known at declaration. For a list of any length, use `Any(column, slot.Expr())` with an `ArrayOf` slot. Configure the registry's `ArrayArg` for the driver when it needs an array codec. `Like` and `ILike` preserve PostgreSQL pattern syntax, including wildcards in a bound pattern.

## Typed relation declarations in v0.4

Set `RowsSpec.Lock = ForRows(strength, rel, wait)` on a single-table plan and execute `plan.Bind(session).Find` with its declared filters and page. Request `WithExists` on an unlocked row plan for a boolean result. For advisory locks, choose the bigint or two-integer overload and run the operation inside a transaction. When moving a function call to `DefineRoutine`, declare its exact signature, cardinality, effect and security mode. Bind the session once, then call `One`, `All` or, for an advisory lock, `Acquire`. Add per-shape requirements to role readiness and use keyed `ReadinessNeeds` literals for the added fields. [Lock and function examples](COOKBOOK.md#registered-locks-and-functions)

Name a physical table by schema and relation. Declare `TableSpec.RelationScope` for a table whose rows depend on database RLS or are shared globally. A table with a tagged scope column can use `ByScopeColumn()` directly. Declare one table occurrence for each single-table plan. Use its fields and slots consistently in the plan and pass runtime values with `BindValue`. [Compilation and role verification](DESIGN-v0.4.md#compilation-binding-and-snapshots)

Typed SQL now spells out each slot's catalog type, including a column-scoped parameter. Refresh stored SQL snapshots and rerun per-role PREPARE and EXPLAIN. A domain or enum retains its own type identity. Declare a bounded character column with `Varchar(n)` to verify the length in `pg_attribute.atttypmod` alongside its type OID. Contradictory declarations for a column fail during registration. PostgreSQL returns zero matches for an overlength equality parameter and applies its native length check on write, including SQLSTATE `22001`. Parameter casts always use the type without its length modifier. [PostgreSQL character types](https://www.postgresql.org/docs/17/datatype-character.html)

### Enable registered SQL by role

Create each strict registry with `Deny: tabula.DenyLegacySQL`. Pass `reg.SQLAuthority()` to its runner or Direct pool during startup. First migrate string predicates, raw statements and dynamically selected columns to registered plans. Regenerate the role's SQL snapshot after switching because the fixed count, list and grouping statements now include the actual scoped and unscoped live-row variants. The zero capability keeps existing registrations and executions available. Use keyed literals for extended option structs. [Registered execution](AUTHORIZATION.md#registered-execution)

Move one single-table operation at a time to a named row plan. Declare the inputs, optional predicates, assignment sets and allowed pages at startup, then bind the plan to an `AuthorizedSession`. Use `RowsFrom(table, rel)` for global or RLS-enforced relations. For a nonstandard scope or soft-delete Go type, pass its `FieldOf(...).Column()` to declare the exact catalog type. Regenerate the snapshot, run source inventory with the new role assignments and verify the bound statements on the migration-built database. Use keyed `sqlcheck.Statement` and `SQLBinding` literals to accommodate their additional fields. [Row plan examples](COOKBOOK.md#registered-single-table-reads-and-writes)

## From v0.3.10 to v0.3.11

Existing keyed `TableSpec` declarations continue to use `WriteDefault`, including the `ReadOnly` and `AllowDelete` flags. The signatures of `Define`, `TryDefine` and bound methods are unchanged. Convert positional `TableSpec` literals to keyed fields before upgrading because the struct gains two fields.

- For an update role, set `WriteMode: tabula.WriteUpdateOnly`. Supply `UpdateColumns` for a column-level grant. Include the version column for versioned rows and the deletion marker when using `SoftDelete`. Reads and locks require `SELECT` on all declared columns.
- For an append role, set `WriteMode: tabula.WriteInsertOnly`. A serial key is generated in PostgreSQL. Remove any dependency on reading the new key, explicit `RETURNING`, or column-targeted conflict handling before adopting this mode.
- Move hand-written `Rows.Exec`, `Scan`, `Select` and `SelectIter` calls off a restricted binding before switching modes. These methods return `ErrOperationDenied` before driver I/O. Review any separate session or view against its PostgreSQL role.
- Regenerate the SQL snapshot and compare each role's grants. Run the registered statements through `VerifyStatements` and exercise serial inserts as the append role. [Table write modes](AUTHORIZATION.md#table-write-modes)

## From v0.3.9 to v0.3.10

If a test login needs roles created by the template installer, call `fixture.Prepare(ctx, plan)` at process startup. Then pass those roles in `LoginRoleOptions.MemberOf` to `AcquireLoginRole` and use `CloneWithLease` for each test. Preparing an existing template refreshes its last-use mark for pruning. Suites whose roles already exist can continue acquiring the login directly. [Cold-start example](AUTHORIZATION.md#process-login-roles)

## From v0.3.8 to v0.3.9

To identify a test worker in PostgreSQL, supply `LoginRoleOptions.Label` in a keyed options literal. Choose up to 25 ASCII lowercase letters, digits or underscores. The fixture rejects invalid labels. The test suite chooses and normalizes its label before acquisition. Empty preserves the v0.3.8 login format.

For crash recovery, replace `fixture.Clone(ctx, plan)` with `fixture.CloneWithLease(ctx, plan, login)`. Keep normal per-test `Database.Drop` cleanup. Use the same maintenance database for acquisition, cloning and pruning. `PruneClones(ctx, 0)` collects eligible clones immediately, including after their login role has been removed. The existing `Prune(ctx, maxIdle, ...)` applies its idle grace to leased clones. Ordinary and historical clones continue to use explicit cleanup. [Example and crash boundary](AUTHORIZATION.md#process-login-roles)

## Process fixture logins in v0.3.8

A test suite that shares a disposable PostgreSQL cluster can acquire a login per process. Use `Fixture.AcquireLoginRole` with the exact member roles, retain the handle for the suite lifetime, and grant CONNECT to its generated name on each clone. Close the client pools, then call `Release` at suite shutdown. An explicit `Prune` on the same maintenance database also reaps roles left by interrupted workers once they have been observed idle for maxIdle. Its return value still lists database names. Use `PruneLoginRoles` to collect role names from a standalone login cleanup. [Example and connection requirements](AUTHORIZATION.md#process-login-roles)

## From v0.3.6 to v0.3.7

Custom tracers keep their existing `Tracer.Start` signature. By default, the error passed to their finish function carries the class and SQLSTATE. Retrieve them with `tabula.ClassifyError` and `tabula.SQLState`. Inspect driver types on the error returned by the database operation. Update log queries that relied on driver wording to use `tabula.error_class` or `tabula.sqlstate`. Applications supplying `Sanitize` continue to control the `tabula.error` log field. Set `RawErrorDiagnostics: true` on `stdsql.RunnerOptions`, `pgxdriver.RunnerOptions` or `RegistryConfig` to deliver the original error to its tracer. With this option the OpenTelemetry event and status description contain its text, which can include bound values for 22P02 or 22003. Metrics continue to use the class in either mode. Use keyed literals when constructing `TraceInfo` directly. [Telemetry fields](LOGGING.md)

Set `RequireCallbackContext: true` on either runner to enforce callback ownership. Pass the context received by each callback into session methods. Derived deadlines, tracing spans and cancellation work normally. A retry or pre-commit step receives a fresh context. Savepoint operations use the enclosing callback context, and both handles in a shared callback accept its context. A detached context can start independent work.

At startup, pass descriptor privilege requirements and additional role requirements to `sqlcheck.CheckReadiness`. An empty result and nil error mean all requested capabilities are present. Keep the existing workload, grant and policy audits for deployment verification. [Role readiness](AUTHORIZATION.md#check-a-role-at-startup)

## From v0.3.5 to v0.3.6

Both drivers expose the same runner options. Set `SingleStatement: true` for a boundary check alone, or configure the methods your application uses.

```go
opts := stdsql.RunnerOptions{
    Statements: tabula.StatementPolicy{
        Read:             []string{"select"},
        Write:            []string{"insert", "update", "delete"},
        RequireReturning: true,
    },
}
```

`Scan` and `Each` use the read list. `Exec` and `Returning` use the write list. A nonempty list activates single-statement checking and denies methods on a side with no allowed commands. To allow CTEs, include `"with"` alongside their outer command. Checks apply to descriptor methods, raw session forwarding methods, shared handles, savepoints and pre-commit steps. Use `policy.Compile(false)` and `guard.Check(method, sql)` for the same checks on an offline SQL inventory. `tabula.GuardSingleStatement` performs the standalone boundary check.

Both Direct pools already enforce a single read query. Their shared guard now rejects ordinary-string quote escapes that depend on `standard_conforming_strings`. Use a bind parameter for that value or an explicit PostgreSQL escape string.

Use `errors.Is(err, tabula.ErrCommit)` to identify an error returned by the outer commit call. The error also wraps the original driver cause. For example, a deferred constraint violation retains its SQLSTATE and PostgreSQL rejected the transaction. After losing a reply, look up the result by the original operation identifier before deciding whether to retry. The default retry policy still uses serialization failures and deadlocks as its criteria.

## From v0.3.4 to v0.3.5

- Configure `InventoryConfig.RoutineMethods` (`SQLRoutineMethod`) to inventory database wrappers that call PostgreSQL functions by name with a fixed argument-slice parameter. A nil or dense slice literal supplies its arity, including when variadic result destinations follow. `+` string concatenations involving single-assignment local string variables now also resolve statically in `CollectSQL`.
- In `tabula check-policies` (`ClassifyPolicies`), `PolicyRule.Expression` and `NotExpression` match or exclude regular expressions across `Using + "\n" + Check` without splitting categories across `USING`-only and `WITH CHECK`-only policies.
- `CheckPolicyCoverage` now infers single-table access across `IS [NOT] DISTINCT FROM` and parenthesized `FROM` expressions, and accepts `"NoAccess": true` on reviewed complex statements that touch no caller-RLS relations.

## From v0.3.3 to v0.3.4

Test suites can call `fixture.Prune(ctx, 7*24*time.Hour, currentPlan)` after creating a fixture to retain the current installation and remove idle cached templates. Choose the retention interval in the test suite. Enable pruning once every worker sharing the cache uses v0.3.4 or later and the same maintenance database on the disposable cluster. The maintenance role needs permission to comment on the templates. The host's `Backend.Drop` handles templates, stages and per-test copies, including any template flags or resources it owns.

Existing templates acquire a server-time mark on their next `Clone`. Pruning preserves templates with missing or invalid marks. After all old workers have stopped, any leftover v0.3.3 `tabula_stage_` databases can be removed during offline test-cluster cleanup. New stages use `tabula_stg_` followed by the template's digest.

## From v0.3.2 to v0.3.3

Active session guards now reject updates to the PostgreSQL `pg_settings` view. Set runner-owned parameters during transaction setup. Existing configurations keep their previous read policy.

To restrict direct reads of an application's setting namespace, compile `NewSessionGuard` once with `VariablePrefixes` (for example `[]string{"example."}`) and `ForbidVariableReads: true`. Pass the guard to your adapter's SQL entry point or the runner's `ScopePolicy.GuardQuery`. Protected names, dynamic setting names, `SHOW ALL` and reads of the settings view return `ErrForbiddenStatement`. A literal setting outside the protected prefix, such as `TimeZone`, remains readable. A column label such as `SELECT 1 AS pg_settings` is allowed. [Session guard guarantees](../SECURITY.md#security-design-guarantees)

## From v0.3.0 to v0.3.2

v0.3.2 includes the untagged v0.3.1 candidate. Upgrade directly from v0.3.0. This release moves role pinning, timeouts, managed scope variables, session sealing, raw-tx opt-in, session-level observability, aggregates and composed view queries into Tabula. The following changes apply to `stdsql` and `pgxdriver` callers.

| v0.3.0 | v0.3.2 | Notes |
| --- | --- | --- |
| `stdsql.NewRunner(db, policy)` | `stdsql.NewRunner(db, policy, stdsql.RunnerOptions{...})` returning `(*Runner[S], error)` | Validates role requirements, timeout bounds, `Unscoped` mode and `ManagedVars` at construction. |
| `pgxdriver.NewRunner(pool, policy)` | `pgxdriver.NewRunner(pool, policy, pgxdriver.RunnerOptions{...})` returning `(*Runner[S], error)` | Same validation and role/timeout/GUC guarantees as `stdsql`. |
| `ScopePolicy.SetupTx` | `ScopePolicy.ManagedVars` + `ScopeVars` + `ContextVars` + `UnscopedVars` + `RunnerOptions.Role` + `RunnerOptions.Timeouts` | The runner applies the role and settings during transaction setup and guards managed variables and timeouts. `Direct` rejects policies declaring managed variables. |
| `sess.Tx()` | `RunnerOptions{ExposeTx: true}` + `runner.WithinRaw` / `WithinRawOptions` / `WithinRawTxOptions` → `rawSess.Tx()` | Default `*Session[S]` no longer exposes `*sql.Tx` / `pgx.Tx`. Raw escape sites are opt-in and reported by `tabula inventory` (`AuditSecurityCalls: true`). |
| A read-only transaction with an empty policy could defer its first snapshot until user SQL | Both runners take the first snapshot during setup and guard the current and default read-only modes | Select the isolation level in the transaction options. PostgreSQL requires snapshot import before the first query, so a read-only callback cannot import a snapshot. Use literal keys for application `set_config` calls on a guarded read-only session. |
| Unscoped sessions permitted by default on `Runner` | `RunnerOptions.Unscoped` defaults to `UnscopedDenied` | Use `UnscopedReadOnly` for reads in read-only transactions. To also read unscoped in a read-write shared transaction, configure a separate `UnscopedReadRole`. Enable `UnscopedReadWrite` only when the workload needs unscoped writes. |
| Nested runner calls allowed | Nested `Within*` using a transaction context returns `tabula.ErrNested` before `BEGIN` | Use `Savepoint` for nested work, or `tabula.WithoutTransaction(ctx)` only for deliberate independent transactions (beware connection pool deadlock). |
| Issuing SQL with an open row cursor depended on the driver | Overlapping transaction statements and savepoint requests return `tabula.ErrSessionBusy` | Collect the IDs you need, finish or break the iteration, then query on the same transaction. |
| Retained session handles stayed callable | Handles close automatically when the callback returns (including errors/panics), returning `tabula.ErrSessionClosed` | Use `RunOptions.BeforeCommit` for pre-commit work on a fresh handle. |
| Registry hooks only | `RunnerOptions.Tracer` / `Metrics` / `Log` / `TagStatements` take precedence per signal over `RegistryConfig` | Each enabled signal has one owner so descriptor statements are never double-emitted; hand-written inline SQL is also observed. |

To adopt **sealed sessions (A4)** incrementally:
1. Construct package-level registries with `RequireSealed: true`.
2. During startup, allocate `key := tabula.NewSessionKey()`, pass `SessionKey: key` into every `RunnerOptions` (and `stdsql.DirectSessionKey(key)` / `pgxdriver.DirectWithConfig(..., DirectConfig{SessionKey: key})`), and call `reg.Seal(key)` checking the error.
3. Unsealed handles (including `NewSession` migration wrappers) fail closed with `tabula.ErrForeignSession` on keyed registries.

For applications using the v0.3.1 preview, regenerate composed-query snapshots with `RenderAllSnapshot` and rerun `tabula inventory`. A resolved descriptor name is part of the site's identity, so review the bindings for changed IDs. Existing SQL snapshots remain readable by `tabula check`. [Role workload configuration](AUTHORIZATION.md)

### Pagination, aggregates and shared sessions

- Check the error returned by `Rows.ListKeyset` for invalid limits, directions, sort columns, cursor lengths and lock modes. Remove any panic recovery used for request validation. The limit must leave room for the extra row used to compute `HasMore`.
- Set `QuerySpec.MaxOffset` on composed lists to bound offset scans. A positive value includes its endpoint. Zero preserves the previous unlimited offset.
- To audit scalar aggregates, list columns per function in `TableSpec.Aggregates`, regenerate the SQL snapshot and run the role inventory. Existing runtime calls and explicit workload variants continue to work.
- For unscoped reads alongside scoped writes, grant a dedicated reader role access to the required rows and configure `UnscopedReadRole` on a runner with `UnscopedReadOnly` and a distinct `Role`. The runner restores `Role` before committing so deferred invoker triggers see the scoped role even after an unscoped read. [Role configuration](AUTHORIZATION.md#using-a-reader-role-in-a-shared-transaction)
- With `iter.Pull2`, defer `stop()` in the callback that consumes the rows. Both drivers close the database cursor when the transaction or savepoint callback returns. Calling `stop` releases the suspended Go iterator.

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
