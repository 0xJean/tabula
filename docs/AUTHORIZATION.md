# PostgreSQL authorization and RLS verification

Tabula v0.3 lets an application audit its configured SQL call sites against a disposable installation of its PostgreSQL schema. The application owns the roles, the mapping from entry points to roles, the expected grants, policy categories, exceptions and test data. Tabula supplies the collectors, comparators and rollback-based probes.

## Check a role at startup

Pass the actual PostgreSQL role bound by the runner. Combine descriptor requirements with the minimum capabilities that the application needs at startup.

```go
derived, _ := sqlcheck.CollectPrivilegeNeeds(registry.Defined())
missing, err := sqlcheck.CheckReadiness(ctx, db, role,
    sqlcheck.ReadinessNeeds{Privileges: derived},
    sqlcheck.ReadinessNeeds{
        SchemaUsage:     []string{"public"},
        AnyColumnSelect: []string{"public.items"},
        ForceRLS:        []string{"public.items", "public.notes"},
        SequenceUsage:   []string{"public.display_order"},
    },
)
if err != nil {
    return fmt.Errorf("check role %q: %w", role, err)
}
if len(missing) > 0 {
    return fmt.Errorf("role %q lacks required capabilities: %v", role, missing)
}
```

One read-only query checks effective grants for the named role, including inherited and PUBLIC grants. It reads PostgreSQL catalogs without assuming that role or executing the named functions. Use qualified relation names and function signatures such as `public.find_item(integer, public.item_kind[])`. Supply schema and column names as they appear in the catalog. Use `Privileges.Columns` for named column grants, or `TableSelect`, `TableInsert`, `TableUpdate` and `TableDelete` for table-level privileges (`pg_catalog.has_table_privilege`). `AnyColumnSelect` requires access to at least one column. `ForceRLS` checks the ENABLE and FORCE flags independently of SELECT access. Owned-sequence requirements come from `Privileges.Sequences`.

A registered existence check requires access to its scope, soft-delete and predicate columns. A check that references no columns needs SELECT on any one column of its table.

A locking read needs `SELECT` on the columns it references and `UPDATE` on any column of each locked table. Pass `StatementShape.AnyColumnUpdate` to `ReadinessNeeds.AnyColumnUpdate`. A missing grant yields `UPDATE ANY COLUMN`. PostgreSQL also filters lockable rows through both the SELECT and UPDATE row-security policies. [Lock privileges](https://www.postgresql.org/docs/17/sql-select.html) · [Policies by command](https://www.postgresql.org/docs/17/sql-createpolicy.html)

`RoleStatements.Readiness` collects locking and routine requirements from the selected shapes. When assembling the check manually, pass the shape's `Routines` to `ReadinessNeeds.Routines`. The check resolves schema, function name and ordered input types in the catalog and checks the result type, scalar or set cardinality, invoker or definer mode, and a read declaration's STABLE or IMMUTABLE volatility. Include the shape's `SchemaUsage` and `Privileges` to verify schema access and EXECUTE on the exact overload. Review function bodies and declare any caller-side table dependencies in the application's role workload. [Function catalog](https://www.postgresql.org/docs/17/catalog-pg-proc.html)

An absent role returns one role finding. Absent objects and missing grants appear in the sorted results. A canceled query or database error fails the check. The application owns its startup deadline, role selection and response to findings. RLS policy behavior and excess grants have separate audits.

## Registered execution

For a `DefineRead` plan, the inventory records each physical relation and the columns used in its projections, joins, filters, groups and ordering. Two aliases of the same table contribute one combined grant set. A source that contributes rows without a referenced column requires SELECT on any column. Every `ByRLS` relation retains its own ENABLE/FORCE requirement. Include unmatched outer rows and missing grants when exercising the plan under its assigned role. [Explicit joins](COOKBOOK.md#explicit-joins)

Enable `DenyLegacySQL` on a registry and pass its verification credential to each runner serving the corresponding role.

```go
reg := tabula.NewRegistry(tabula.RegistryConfig[int64]{Deny: tabula.DenyLegacySQL})
authority := reg.SQLAuthority()
runner, err := stdsql.NewRunner(db, scope, stdsql.RunnerOptions{SQLAuthority: authority})
```

Use the same field on `pgxdriver.RunnerOptions`. For a read-only pool, use `stdsql.DirectSQLAuthority(authority)` or `pgxdriver.DirectWithConfig` with `DirectConfig{SQLAuthority: authority}`. The zero credential permits the existing text methods. A registry with a different credential cannot execute its descriptors on a strict session. A session key can additionally restrict which concrete session instances the registry trusts.

The registry signs each call for the receiving session, method, statement and ordered parameters. Registered executions still pass through the session guard, method policy, role and observability hooks. Streams returning rows from a write use the Returning policy. Direct rejects writes, row locks and transaction-level advisory locks before acquiring a connection.

Fixed generated operations include primary-key reads and locks, insert, save, soft delete, physical delete, declared single-row upserts, and declared scalar and grouped aggregates. Registered named-parameter SQL declarations (`DefineSQL`, `DefineSQLExec`, `DefineSQLReturning`, `DefineSQLList`) also sign through the registry's `SQLAuthority` and execute under `DenyLegacySQL`, carrying `AllowedTypes` metadata for bidirectional parameter and result verification. A baseline list or find uses the default ordering and the session's live-row predicate. Strict declarations accept table and column identifiers and default ordering over declared columns with optional direction and null placement. Filter callbacks, run-time string order, dynamic column lists and batch sizes require an explicitly compiled alternative. A fragment-based upsert, `QuerySpec`, hand-written `Statement`, and raw transaction entry are rejected. `ErrLegacySQL` matches `ErrCapabilityDenied`.

PostgreSQL grants and RLS still apply. A separately acquired database connection has its own privileges. The binary COPY protocol retains its descriptor and session checks, including the independent `DenyCopy` capability.

## Physical sources for typed plans

`DefinePlanSource(reg, spec)` registers the table's physical columns and keys. Its baseline privilege list is explicitly empty. `DefineRows` and `DefineRead` record the physical grants on their output view's shapes. `DefineTableRows`, `DefineExists`, `DefineUpdateMany` and `DefineDeleteMany` register their shapes on the source table. Mapping a table alone adds no grants. A partial projection requests SELECT on the fields it reads, including any predicate, order and soft-delete fields. A column-free existence query requires SELECT on any column. Bounded exact and first-row reads inherit the same role, scope and locking requirements as their data query. `CollectPrivilegeNeeds` takes the union across the supplied definitions. Use the role's selected shapes from `RoleStatements.Readiness(role)` for `CheckReadiness`, then verify the same assignments with `VerifyRoleStatements`.

For `UPDATE items SET rank = $1 WHERE id = $2`, the role needs column privileges `UPDATE(rank)` and `SELECT(id)`. A plan that also reads the old rank needs `SELECT(rank)`. Scope-column filters, live-row predicates and version comparisons contribute the columns they read. A `ByRLS` source records the requirement for enabled and forced row security on its physical relation.

A locking read of a table whose columns form a composite primary key needs `SELECT` on the columns it reads and `UPDATE` on any one column. A grant on one key column satisfies the row-lock requirement. The source registers no standalone update. Its primary keys remain protected from typed assignment.

`ReadOnly`, `WriteMode`, `AllowDelete` and `UpdateColumns` constrain plan declarations. The old `Rows` execution methods and COPY are denied before database I/O, with or without `DenyLegacySQL`. Declare additional application requirements separately when combining startup checks. [Declaration example](COOKBOOK.md#physical-sources-with-column-grants)

## Table write modes

Choose a descriptor for each execution role and pass that role's definitions to `CollectPrivilegeNeeds` and `VerifyStatements`. Every role also needs schema `USAGE`. PostgreSQL evaluates grants, row policies, defaults and triggers when the statement runs.

| Declaration | Derived operations | Derived privileges |
| --- | --- | --- |
| `WriteDefault` (zero) | Existing reads and writes. `ReadOnly` selects reads only. `AllowDelete` enables physical deletes. | Existing column grants, sequence usage for writable serial tables, and table `DELETE` when enabled |
| `WriteUpdateOnly` | Read live rows, lock rows and update the permitted columns | `SELECT` on all descriptor columns and `UPDATE` on the permitted columns |
| `WriteInsertOnly` | Insert, batch insert, binary COPY with input columns, and an explicitly declared targetless `ON CONFLICT DO NOTHING` | `INSERT` on bound columns and `USAGE` on the owned sequence of a declared serial key |

**Updating selected columns.** Set `UpdateColumns` to a nonempty list, or omit it to permit all updatable columns. `Save`, `SaveReturning` and `UpdateWhere` assign the selected columns in descriptor order. Key, scope and `once` columns are protected. Include the version column on versioned rows so these methods can compare against the previous version. Existing single-row `Set`, `SetWhere` and `SetExpr` retain their individual version and target rules. `SoftDelete` requires permission to update the deletion marker. Rows already marked deleted stay outside the live-row operations. `SaveReturning` can return any declared column because this role has the corresponding `SELECT` access.

**Appending rows.** `Insert`, `InsertMany` and COPY use the normal insert inputs, including scope, `once` and version values. A timestamp deletion marker uses its database default. Boolean and status markers use the supplied value. With a serial key, the server generates the key and the caller's field stays untouched. If all columns take defaults, the SQL names the serial key with a literal `DEFAULT`, which requires `INSERT` on that key. COPY requires at least one bound input column. An explicit key uses its column's `INSERT` grant and adds no sequence requirement.

For duplicate suppression, declare `entries.DefineUpsert("append_once", tabula.UpsertSpec{})`. Its registered SQL uses `ON CONFLICT DO NOTHING`, which can skip a conflict on any usable unique constraint. An explicit conflict target requires reading the target columns. A `RETURNING` list requires `SELECT` on the returned columns. Strict insert descriptors reject both, along with `DO UPDATE`, reads, locks and deletes. `InsertReturning` is rejected even for an empty list, which normally returns every column.

The two explicit modes reject hand-written `Rows.Exec`, `Scan`, `Select` and `SelectIter`. A mode denial matches `tabula.ErrOperationDenied`. Registry capabilities, session provenance, read-only transactions and scope checks still apply. Explicit `Statement` and `Requires` declarations add application-owned audit entries. SQL issued directly on a session or through another descriptor is governed by that handle and the PostgreSQL role. Verify registered statements under the bound role and exercise inserts to check sequence access at execution time.

## Collect the workload

Run `tabula inventory --config inventory.json` in the application checkout. `Dir` is relative to the working directory. For example, this configuration collects the SQL argument of `(*sql.Tx).QueryContext` and `(*sql.Tx).ExecContext`.

`inventory.json`

```json
{
  "Dir": ".",
  "Patterns": ["./..."],
  "Methods": [
    {"Receiver": "database/sql.Tx", "Method": "QueryContext", "Argument": 1},
    {"Receiver": "database/sql.Tx", "Method": "ExecContext", "Argument": 1}
  ]
}
```

Specify each adapter or interface that accepts SQL. Argument positions start at zero after the method receiver. Use `RoutineMethods` (`NameArgument`, `ArgsArgument`, optional `Schema`, `SetReturning`) for helpers that invoke PostgreSQL functions. Configure the fixed parameter positions for the name and the database argument slice. A known name and a nil or dense slice literal determine the SQL, even when the helper also takes variadic result destinations. A variadic database input, a sparse literal or a runtime slice requires review. The collector uses the Go toolchain, build tags and type information. It resolves aliases, method expressions, imported string constants, constant concatenations and locals with a single constant initializer (including `+` concatenations containing single-assignment local strings). A dynamic argument or a method value passed elsewhere produces a finding. An application can register its site ID and a review reason in `Exceptions`. A removed site reports a stale exception. Supply representative SQL variants for registered builders in the execution workload.

The default inventory covers production sources selected by the current Go build configuration. Use `Tests: true` for test files and `Tags` for optional code. Run once per supported build configuration. Calls through interfaces need the interface receiver in `Methods`. Site IDs survive line moves and distinguish repeated calls. An ID changing after an edit calls for reviewing its role assignment and exception.

Assign each site ID to an entry such as `{"Roles":["docs_reader"]}` in a bindings file. Run `tabula inventory --config inventory.json --bindings bindings.json > workload.json` to collect statements and check their assignments together. `SQLInventory.BindRoles` reports missing or retired assignments. A dynamic entry supplies named `Variants` with their SQL. An explicitly skipped site requires a registered exception. A static site can supply `Access` for command coverage. The same site can be assigned to several roles.

Inventory records declarations and uses of typed row locks, transaction advisory locks and routine calls. For an indirect plan, supply its owning `Definition` and `Plan`. The registered effect and result cardinality must match the source method. A row lock contributes both SELECT and UPDATE policy access. A routine needs reviewed `Access` or `NoAccess` for its body. Role readiness uses its exact function signature, and a dedicated advisory lock records the privilege for its built-in overload.

For composed queries and aggregates, add `--snapshot statements.sql`, generated with `sqlcheck.RenderAllSnapshot(reg.Defined(), reg.DefinedViews())`. The collector resolves literal descriptor names in direct declarations and single-assignment local or private package handles. A query key such as `list` can be reused on several views. Each role receives the variants registered for its own view and query. An optional `Prefix` selects a subset of those variants.

Use `Definition` for an injected, mutable or exported handle. For example, another package can reassign the exported variable `Items` below. Here `reg` has scope type `struct{}` and `item` maps the `id` key and the `kind` column.

`items.go`

```go
var Items = tabula.Define(reg, tabula.TableSpec[item, int64]{
	Name:     "public.items",
	ReadOnly: true,
	GroupBy:  []string{"kind"},
})

func CountsByKind(ctx context.Context, sess tabula.Session[struct{}]) (map[string]int64, error) {
	return tabula.CountBy[string](ctx, Items.Bind(sess), "kind", nil)
}
```

Copy the `CountBy` site's `ID` from `tabula inventory --config inventory.json` into the bindings file. Set `Definition` to the descriptor's `TableSpec.Name`. This assigns the registered `public.items/count_by_kind` statement to `items_reader`.

`bindings.json`

```json
{
  "<CountBy site ID>": {"Roles":["items_reader"], "Definition":"public.items"}
}
```

A missing definition or statement fails the command with the site ID. Declare the pairs needed for automatic scalar verification with, for example, `Aggregates: tabula.ScalarAggregates{Max: []string{"id"}}` in `TableSpec`. The snapshot records the query for live rows and, on a scoped table, both scope variants. Automatic scalar verification requires a literal `nil` filter at the call site. Recollect any inventory saved by an earlier version. Supply named `Variants` for calls with a filter callback, undeclared scalar aggregates, unregistered `Exists` statements, queries compiled with `TryDefineQuery`, or helpers used across several descriptors. Put explicit `Access` requirements on each manual variant. To skip a site verified elsewhere, register the reason in `Exceptions` and use `{"Skip":true}` as its binding.

Consumers can also supply ordinary named statements under each execution role. Both routes use the same workload format.

`workload.json`

```json
{
  "docs_reader": [
    {"Name": "read-note", "SQL": "SELECT id FROM docs.notes WHERE id = $1"}
  ],
  "docs_writer": [
    {"Name": "add-note", "SQL": "INSERT INTO docs.notes(id) VALUES($1) RETURNING id"}
  ]
}
```

`tabula verify-roles --config workload.json` uses `SET LOCAL ROLE`, `PREPARE` and a generic `EXPLAIN EXECUTE` with inferred parameter types. Existing descriptor-based `VerifyStatements` and `VerifyViewStatements` remain available. Database commands below take `--dsn` or read `TABULA_DSN`. The CLI exits 0 on success, 1 for findings and 2 for input or operational errors.

For result-bearing writes, pass `pgxdriver.SQLVerifier(pool)` to `sqlcheck.VerifyRoleStatementsUsing` or `VerifyStatementsUsing`. The backend describes the original statement without execution, including modifying CTEs and multiple row-lock clauses. It verifies role setup, catalog facts, parameter and result descriptions, and EXPLAIN on one connection, then rolls back. The runtime driver can remain `stdsql`. The portable `database/sql` verification path and CLI return `ErrDescribeRequired` when non-executing description is necessary; do not substitute a `LIMIT 0` query around a modifying statement.

`RoleStatements.Readiness` includes structural write requirements. `UniqueMatches` verifies immediate unique keys with non-NULL columns for write-source matching. `Defaults` checks actual sequence dependencies, including every physical column omitted from an INSERT; identity generation has separate PostgreSQL semantics. `Conflicts` checks catalog eligibility, while statement verification also checks the actual generic EXPLAIN arbiter list. These checks complement execution tests for triggers, default functions and policies. Logical domain results need catalog origin or independently established cast evidence; a base wire OID alone does not establish a domain.

## Using a reader role in a shared transaction

For a job that reads shared rows then writes scoped rows atomically, give the login permission to switch to two roles. Grant the reader only the required schema usage, reads and reviewed functions. Check its inherited privileges, ownership and executable functions as well as direct table grants. Grant scoped writes to the writer and enforce scope with the application's RLS policies.

```go
opts := stdsql.RunnerOptions{
    Role:             "records_writer",
    Unscoped:         stdsql.UnscopedReadOnly,
    UnscopedReadRole: "records_reader",
}
```

Inside `WithinShared`, use `sh.Unscoped()` for reads and `sh.Scoped(scope)` for writes. A scope recognized by `IsUnscoped` gets the same reader handle. Both use the same connection and transaction. Before each statement the runner selects the handle's role with `SET LOCAL ROLE` and reapplies managed scope variables. After the callback succeeds and its handles close, the runner restores `Role` before `COMMIT`. A deferred invoker trigger runs under that role even when the last statement was an unscoped read. A trigger that needs the scope of a particular write can read it from the triggering row. This option adds one role-selection round trip per statement and one before commit in both drivers. Assign each handle's queries to its effective role when verifying the workload.

## Report grant drift before narrowing access

Select schemas and direct grantees in `scope.json`. This scope includes the two application roles and grants made to everyone. It covers all relations in the selected schema, including objects with no Tabula descriptor.

`scope.json`

```json
{
  "Schemas": ["docs"],
  "Roles": ["docs_reader", "docs_writer", "PUBLIC"]
}
```

```sh
tabula catalog --config scope.json --section grants.json
tabula grants --config privileges.json
```

A grant manifest contains the same scope plus its expected entries. This small declaration expects exactly one column grant for one role.

`privileges.json`

```json
{
  "Schemas": ["docs"],
  "Roles": ["docs_reader"],
  "Grants": [
    {"Role":"docs_reader","Schema":"docs","Kind":"COLUMN","Object":"notes","Column":"id","Privilege":"SELECT"}
  ]
}
```

`CheckGrantManifest` reports `Missing` and `Extra` and generates SQL to reconcile them. It compares direct ACLs on tables, views, columns, sequences and overloaded functions. A function's `Arguments` uses the catalog's qualified type names, such as `pg_catalog.int4`. Grant options participate in equality. The generated order restores surviving column grants after a table-level revoke. Grantors are aggregated. Ownership, role membership, schema `USAGE`, database `CONNECT` and future-object default privileges are separate inputs to effective access. Use the role's planning and behavior probes to measure that access.

The `FUNCTION` entries in `grants.json` cover ordinary, aggregate and window functions. Procedure `EXECUTE` grants are outside the scope of grant comparison and minimization in v0.3.

### Table-level grant granularity

By default, `GrantGranularity` is `GranularityAll` (`""`), which audits `TABLE`, `COLUMN`, `SEQUENCE` and `FUNCTION` ACLs. Projects that manage privileges at table granularity instead of maintaining per-column ACL manifests can select `GranularityTable` (`"table"`):

- Set `"Granularity": "table"` in `CatalogScope` (`scope.json` / `privileges.json`) or pass `--granularity=table` to `tabula catalog` and `tabula grants`. `ReadCatalog` skips `pg_attribute.attacl`, and `CompareGrants` / `CheckGrantManifest` reject manifests containing `Kind == "COLUMN"` entries.
- Set `"Granularity": "table"` in `MinimizeConfig` (`trials.json`) or pass `--granularity=table` to `tabula minimize`. Trial revocation minimizes direct `TABLE`, `SEQUENCE` and `FUNCTION` grants without expanding column-level grants.
- For startup readiness checks at table granularity, declare required relations in `ReadinessNeeds.TableSelect`, `TableInsert`, `TableUpdate` and `TableDelete`, which check `pg_catalog.has_table_privilege` alongside `SchemaUsage`, `SequenceUsage`, `ForceRLS` and `Routines`.

## Trial each revocation

Pass one role, its schemas and its named statements to `tabula minimize --config trials.json`. The maintenance connection needs authority to assume the role and alter its grants in a disposable clone.

`trials.json`

```json
{
  "Role": "docs_reader",
  "Schemas": ["docs"],
  "Statements": [
    {"Name":"read-note","SQL":"SELECT id FROM docs.notes WHERE id=$1"}
  ]
}
```

`MinimizeGrants` reads a repeatable snapshot and verifies the complete baseline. Each trial temporarily revokes one direct grant with `RESTRICT`, reads the ACL back and reruns the workload. Successful removals accumulate within the transaction. A dependent grant or an observed permission denial keeps the candidate. The final workload is verified again and the transaction rolls back. `Current` optionally asserts the expected starting ACL. `Keep` pins host requirements. The output includes the surviving set, removed entries and SQL for review.

The result is inclusion-minimal for eligible entries in the supplied workload and trial order. Broad table grants are tried before column alternatives. The trial removes whole ACL entries that exist at the start. The manifest can express a new column grant or a grant-option downgrade. Overlapping memberships and unrepresented application paths affect what a successful trial proves.

Planning alone cannot establish which sequence defaults, trigger paths or function bodies run. Privileges outside DML planning coverage that pass a trial are retained in `Unverified`. An optional `Runtime` entry supplies a positive SQL execution, bound `Args`, custom `Vars` and the direct grants it `Covers`. Each execution gets its own rolled-back savepoint. Use representative arguments and fixtures. Sequence increments and external effects from database functions can survive rollback.

## Snapshot and classify policies

`ReadCatalog` takes one read-only, repeatable-read snapshot. It uses a fixed deparser search path for policy expressions and function definitions. Keep the PostgreSQL major version fixed when reviewing text snapshots.

```sh
mkdir -p testdata/catalog
for section in policies.json force-rls.json grants.json security-definers.json; do
  tabula catalog --config scope.json --section "$section" > "testdata/catalog/$section"
done
tabula catalog --config scope.json --against testdata/catalog
```

The four lists contain policy identities and predicates, FORCE RLS relations, direct grants and SECURITY DEFINER routines. The `security-definers.json` snapshot records the owner, signature, settings and definition of each SECURITY DEFINER ordinary function, window function and procedure in the selected schemas. A changed entry shows up even when the total count stays the same. Treat routine bodies as application source.

`tabula check-policies --config policies.json` accepts the catalog scope plus `Rules`. A rule can specify `Command`, `Permissive`, an exact role set, identifier patterns (`SchemaPattern`, `TablePattern`, `NamePattern`) and Go regular expressions (`Using`, `Check`, and `Expression` / `NotExpression` over `Using + "\n" + Check`). Conditions are combined. `Entries` restricts a category to registered schema/table/policy identities. `UnregisteredOnly` lets a general category exclude the exception registry.

`policies.json`

```json
{
  "Schemas":["docs"], "Roles":["docs_reader","docs_writer"],
  "Rules":[
    {"Name":"reader","Command":"SELECT","Roles":["docs_reader"],"UnregisteredOnly":true},
    {"Name":"writer","Command":"INSERT","Roles":["docs_writer"],"UnregisteredOnly":true},
    {"Name":"registered","Permissive":false,"Entries":[{"Schema":"docs","Table":"notes","Name":"scope_fence"}]}
  ]
}
```

Each policy must match exactly one rule. Unmatched policies, overlapping matches and registered entries that no longer satisfy their rule are findings. `CheckRLS` and `tabula check-rls` provide the simpler table-level preset.

## Check command coverage and exercise rows

For a typed statement with no registered table access, review the routine
body and declare its caller-context `Access`, or set `NoAccess` after that
review. Role binding and policy coverage enforce the declaration even for a
scalar function called without a `FROM` clause. The dedicated PostgreSQL
transaction advisory lock has no table dependency. Row locks require SELECT
and UPDATE policy coverage, together with UPDATE on at least one column.

`tabula check-coverage --config workload.json` checks catalog policy coverage on RLS-enabled tables. It accounts for permissive policies, `ALL`, `PUBLIC`, inherited roles, FORCE and role bypass. `INSERT … RETURNING` requires SELECT coverage. In the example workload, a writer with the required column grants and only an INSERT policy can pass `verify-roles`. Running `check-coverage` then reports a missing SELECT policy for `add-note`. Targeted `ON CONFLICT` needs SELECT coverage and `DO UPDATE` also needs UPDATE.

A simple read such as `SELECT substring(note FOR 3) FROM docs.notes` requires SELECT coverage. Adding a top-level `FOR UPDATE`, `FOR NO KEY UPDATE`, `FOR SHARE`, or `FOR KEY SHARE` clause also requires UPDATE coverage. Null-safe comparisons (`IS [NOT] DISTINCT FROM`) and parenthesized function arguments (`EXTRACT(... FROM ...)`, `TRIM(... FROM ...)`) are recognized inside single-table statements. For joins, CTEs, multi-source writes and other complex SQL, supply the full reviewed `Access` list on the statement, for example `[{"Table":"docs.notes","Command":"SELECT"}]`, or set `"NoAccess": true` when a reviewed complex statement (such as a `SECURITY DEFINER` routine call) accesses no caller-RLS relations. For complex SQL, `NoAccess` records the reviewer’s assessment. Check the routine body, execution role and any views before setting it. Inferred table access conflicts with the declaration. Qualify relations for the application search path. Pair structural checks with role verification so that PostgreSQL also validates the SQL and the referenced objects. [PostgreSQL policy command rules](https://www.postgresql.org/docs/17/sql-createpolicy.html)

`tabula test-rls --config behavior.json` or `RunRLSMatrix` executes the role × scope × statement matrix. Every cell must have one expectation. `Rows` compares a multiset of text values and SQL nulls, or checks order with `Ordered`. `Affected` asserts a write count. `Denied` accepts `rls_denied` or `insufficient_privilege`. Invisible rows commonly yield an empty result or zero affected rows.

`behavior.json`

```json
{
  "Roles":["docs_reader"],
  "Scopes":{"east":{"example.scope":"east"},"west":{"example.scope":"west"}},
  "Statements":{"list":{"SQL":"SELECT id FROM docs.notes ORDER BY id"}},
  "Cases":[
    {"Role":"docs_reader","Scope":"east","Statement":"list","Rows":[["1"]]},
    {"Role":"docs_reader","Scope":"west","Statement":"list","Rows":[]}
  ]
}
```

Each cell gets a new rollback-only transaction with explicit identity and transaction-local custom GUCs. `ClassifyError` maps server diagnostic evidence for SQLSTATE 42501 to `ErrRLSDenied` or `ErrInsufficientPrivilege`. Drivers lacking a recognized RLS diagnostic receive the generic privilege class. `NewSessionGuard` lets an adapter protect its own GUC prefixes and independently forbid role changes, session authorization and `set_config` calls.

## Reuse an installed test database

`postgresfixture.Installation` describes a revision, base bytes and ordered named migrations. Include every input that affects installation. `Through("002_notes.sql")` selects the inclusive prefix. `TemplateName` derives a stable name from the length-framed inputs. The fixture uses an advisory lock on a shared maintenance database, installs into staging, then publishes a complete template. `Fixture.Clone` returns a fresh `Database` with `Name()` and `Drop(ctx)` for test cleanup.

The host supplies a `Backend` that clones and drops databases on the supplied maintenance connection and runs its installer against the staging database. The installer must close its staging connections before returning. Give concurrent workers the same maintenance database on a disposable cluster. Reserve the fixture's `tabula_tpl_<digest>` and `tabula_stg_<digest>` names. Preparing or cloning a template records its last use with the server clock. A clone holds the shared template lock through the physical copy. [Fixture integration test](../postgresfixture/fixture_integration_test.go)

Call `Fixture.Prune(ctx, maxIdle, keep...)` to remove idle templates and abandoned stages. It skips active copies and the templates of the supplied installations. A missing or invalid last-use mark preserves the template. The host's `Backend.Drop` handles template flags and related resources. A later `Clone` rebuilds a pruned template. [Fixture migration notes](MIGRATION.md#from-v033-to-v034)

## Audit login-role hygiene and security call sites

Application login roles that use `SET LOCAL ROLE` through `stdsql.Runner` or `pgxdriver.Runner` should hold no direct object access of their own. Run `sqlcheck.CheckLoginRole` or `tabula check-login-role --role scope_login --member-of scope_reader,scope_writer --schemas public` to verify `LOGIN NOINHERIT`, absence of `SUPERUSER`/`BYPASSRLS`/`CREATEROLE`/`CREATEDB`/`REPLICATION`, exact role memberships (including PostgreSQL 16+ per-membership `WITH INHERIT TRUE` grants), zero direct table or column grants, zero owned objects and no `CREATE` privilege on selected schemas.

Set `"AuditSecurityCalls": true` in `inventory.json` (or `InventoryConfig.AuditSecurityCalls = true`) to collect typed call sites for `NewSessionKey`, `Registry.Seal`, `SessionKey.Sign`, keyed runner/Direct factories, `WithoutTransaction` / `MarkTransaction` / `InTransaction`, and both drivers' raw transaction access (`WithinRaw`, `RawSession.Tx`, `ExposeTx`) across import aliases and escaped function or method values. Field targets include direct selections, fields promoted by embedding, and keyed struct literals.

## Process login roles

Give each process its own login when independent test suites share a disposable cluster. If installation migrations create the member roles, call `Prepare` first. Concurrent workers can prepare the same installation. The first publishes the template and the others refresh its last-use mark. Each process can then acquire its login and use it across test clones.

```go
if err := fixture.Prepare(ctx, plan); err != nil {
    return err
}
login, err := fixture.AcquireLoginRole(ctx, postgresfixture.LoginRoleOptions{
    MemberOf: []string{"fixture_reader", "fixture_writer"},
    Label:    "suite_2",
})
if err != nil {
    return err
}
defer func() { err = errors.Join(err, login.Release(cleanupCtx)) }()

db, err := fixture.CloneWithLease(ctx, plan, login)
if err != nil {
    return err
}
defer func() { err = errors.Join(err, db.Drop(cleanupCtx)) }()

// The host grants CONNECT on db.Name() and closes its clients before returning.
return runSuite(ctx, db.Name(), login.Name(), login.Password())
```

In this example the suite function has a named error result. Supply a live cleanup context at shutdown, and close business pools before releasing the handle. An interrupted worker leaves a versioned role mark. On its first observation of an unlocked login with no client sessions, Prune records the server time. A later sweep can remove that login and its database CONNECT grants after maxIdle. An observed client connection clears the idle mark. Call `PruneLoginRoles(ctx, 0)` for immediate orphan cleanup. Unexpected dependencies are reported and the transaction rolls back. The host owns migration objects, template roles and PUBLIC privileges.

Each live login uses one checked-out maintenance connection even between test cases. Supply a direct or session-pooled connection and leave pool capacity for cloning and pruning. A lost maintenance session ends the lease. Acquire a new login before continuing after a server restart or permanent connection failure. [Lifecycle design](DESIGN-v0.3.8.md)

The optional login label is visible in PostgreSQL and accepts 1–25 ASCII lowercase letters, digits or underscores. Multiple processes can use the same label. Each gets an independent random name and lease.

A live lease keeps its clones between connections. After a worker exits, `PruneClones(ctx, 0)` collects marked clones once all clients disconnect. `Prune(ctx, maxIdle)` applies its usual server-timed grace period. An ordinary clone, or a database left by an interruption between creation and marking, requires explicit cleanup. Automatic clone cleanup uses a non-forcing drop. Suites with host-specific teardown requirements should account for those separately before opting in. [Clone ownership and lifetime](DESIGN-v0.3.9.md)

## Bind a typed row plan to roles

`CollectSQL` records row-plan declarations and the calls to `Find`, `List`,
`Count`, `UpdateMany` and `DeleteMany`. For a direct declaration or a
single-assignment handle it records the owner and plan name. When a factory,
parameter or mutable handle hides them, set `SQLBinding.Definition` and
`SQLBinding.Plan` to the reviewed owner and plan name. A binding can use
`Prefix` to select a registered shape or a dotted key prefix. A `List` site
includes the matching count for each selected data shape.

Typed writes follow the same owner and plan binding. `DefineWrite`,
`DefineReturning`, `DefineTableReturning`, `DefineUpdateReturning` and
`DefineDeleteReturning` register complete families, including their `Try`
partners. `Exec` selects one statement; `ExecBatch` and `AllBatch` select only
the declared row-count forms. `All` and `Iter` select a non-batch result, while
the finite mutation methods select their assignment and filter alternatives.
The output view owns a returned mutation's shapes even when it writes another
physical relation. Intermediate write builders and common tables are not
separate execution sites; the final shape carries every nested effect and grant.

`BindRolesWithDefinitions` expands the registered keys from the live registry
or a parsed SQL snapshot. A `Count` call adds only the count variants. A
`Find` call adds only data variants. Each statement supplies its physical
commands and column privileges. Declaration sites cover the whole family by
default. Assign them to the roles that need those shapes or use a reviewed
inventory exception when separate execution sites account for them.

```go
work, err := inventory.BindRolesWithDefinitions(assignments, tables, views)
if err != nil {
    return err
}
needs, err := work.Readiness("scope_reader")
if err != nil {
    return err
}
missing, err := sqlcheck.CheckReadiness(ctx, db, "scope_reader", needs, extraNeeds)
```

Run `VerifyRoleStatements` to prepare and explain the exported workload under
each assigned role. For typed reads it also checks physical column types,
parameter types, and the projected names and types. The type probe uses a
zero-row source. Run the operations on representative scoped data to verify
row visibility, aggregate NULL results and affected counts.

## Result key verification

`StatementShape.Keys` records each physical primary-key premise used by a joined
keyset or a deterministic DISTINCT ON winner. `Comparisons` records physical
operands whose native types and deterministic collations support the order.
Compound order terms also retain pairs of physical inputs whose implicit
collations must be compatible. Readiness conservatively rejects conflicting
non-default collations even beneath nested expressions; separate order terms
may use different deterministic collations.
These fields survive SQL snapshots and `RoleStatements.Readiness`; they add no
column grants. `VerifyStatements` and `VerifyRoleStatements` verify the same
premises while preparing each statement.

Catalog checks require a complete, valid, immediate primary key using compatible
native ordering. Deferrable keys and ordinary inherited scans with descendants
cannot establish cross-row uniqueness. A valid parent-wide primary key on a
partitioned table is accepted. Grouped and whole-DISTINCT result proofs do not
invent a physical-key requirement. Rerun readiness after schema changes.

List data and exact summaries have independent grants, inputs and observation
keys. Authorize counts separately when a role should see only cardinality. A
plain summary needs join, filter, scope and live-row columns; grouped summaries
also need group and HAVING inputs. DISTINCT summaries retain values determining
the distinct classes. Cursor operators use qualified native implementations and
record their function privileges without requiring internal sort support grants.

## Signed write method policies

`StatementPolicy.AllowAuthorizedWriteCTE` permits a registered write CTE whose outer result is SELECT. Include `with` and each physical DML command in the write allowlist. An upsert requires both INSERT and UPDATE in that list. `AllowAuthorizedWriteRoutine` permits a registered write routine's SELECT-shaped call through Returning; its exact signature and EXECUTE requirement remain part of the immutable shape.

Both flags default to false. They are checked against the original registry-issued call, receiving session and method. Editing a SQL snapshot or `AuthorizedSQL` cannot enable them. SQL text APIs retain their normal command checks, and both drivers still enforce callback ownership, read-only state, session guards and database privileges. A custom driver calls `StatementGuard.CheckAuthorized` for its registered path before performing I/O.
