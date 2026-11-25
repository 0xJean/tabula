# PostgreSQL authorization and RLS verification

Tabula v0.3 lets an application audit its configured SQL call sites against a disposable installation of its PostgreSQL schema. The application owns the roles, the mapping from entry points to roles, the expected grants, policy categories, exceptions and test data. Tabula supplies the collectors, comparators and rollback-based probes.

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

Specify each adapter or interface that accepts SQL. Argument positions start at zero after the method receiver. Use `RoutineMethods` (`NameArgument`, `ArgsArgument`, optional `Schema`, `SetReturning`) for helper methods that invoke PostgreSQL functions by name and slice literal rather than raw SQL strings. The collector uses the Go toolchain, build tags and type information. It resolves aliases, method expressions, imported string constants, constant concatenations and locals with a single constant initializer (including `+` concatenations containing single-assignment local strings). A dynamic argument or a method value passed elsewhere produces a finding. An application can register its site ID and a review reason in `Exceptions`. A removed site reports a stale exception. Supply representative SQL variants for registered builders in the execution workload.

The default inventory covers production sources selected by the current Go build configuration. Use `Tests: true` for test files and `Tags` for optional code. Run once per supported build configuration. Calls through interfaces need the interface receiver in `Methods`. Site IDs survive line moves and distinguish repeated calls. An ID changing after an edit calls for reviewing its role assignment and exception.

Assign each site ID to an entry such as `{"Roles":["docs_reader"]}` in a bindings file. Run `tabula inventory --config inventory.json --bindings bindings.json > workload.json` to collect statements and check their assignments together. `SQLInventory.BindRoles` reports missing or retired assignments. A dynamic entry supplies named `Variants` with their SQL. An explicitly skipped site requires a registered exception. A static site can supply `Access` for command coverage. The same site can be assigned to several roles.

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

`tabula check-coverage --config workload.json` checks catalog policy coverage on RLS-enabled tables. It accounts for permissive policies, `ALL`, `PUBLIC`, inherited roles, FORCE and role bypass. `INSERT … RETURNING` requires SELECT coverage. In the example workload, a writer with the required column grants and only an INSERT policy can pass `verify-roles`. Running `check-coverage` then reports a missing SELECT policy for `add-note`. Targeted `ON CONFLICT` needs SELECT coverage and `DO UPDATE` also needs UPDATE.

A simple read such as `SELECT substring(note FOR 3) FROM docs.notes` requires SELECT coverage. Adding a top-level `FOR UPDATE`, `FOR NO KEY UPDATE`, `FOR SHARE`, or `FOR KEY SHARE` clause also requires UPDATE coverage. Null-safe comparisons (`IS [NOT] DISTINCT FROM`) and parenthesized function arguments (`EXTRACT(... FROM ...)`, `TRIM(... FROM ...)`) are recognized inside single-table statements. For joins, CTEs, multi-source writes and other complex SQL, supply the full reviewed `Access` list on the statement, for example `[{"Table":"docs.notes","Command":"SELECT"}]`, or set `"NoAccess": true` when a reviewed complex statement (such as a `SECURITY DEFINER` routine call) accesses no caller-RLS relations. Qualify relations for the application search path. Review view-owner and function execution contexts separately. Pair structural checks with role verification so that PostgreSQL also validates the SQL and the referenced objects. [PostgreSQL policy command rules](https://www.postgresql.org/docs/17/sql-createpolicy.html)

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

The host supplies a `Backend` that clones and drops databases on the supplied maintenance connection and runs its installer against the staging database. The installer must close its staging connections before returning. Give concurrent workers the same maintenance database on a disposable cluster. Reserve the fixture's `tabula_tpl_<digest>` and `tabula_stg_<digest>` names. Each `Clone` records the server time on its template and holds a shared lock through the copy. [Fixture integration test](../postgresfixture/fixture_integration_test.go)

Call `Fixture.Prune(ctx, maxIdle, keep...)` to remove idle templates and abandoned stages. It skips active copies and the templates of the supplied installations. A missing or invalid last-use mark preserves the template. The host's `Backend.Drop` handles template flags and related resources. A later `Clone` rebuilds a pruned template. [Fixture migration notes](MIGRATION.md#from-v033-to-v034)

## Audit login-role hygiene and security call sites

Application login roles that use `SET LOCAL ROLE` through `stdsql.Runner` or `pgxdriver.Runner` should hold no direct object access of their own. Run `sqlcheck.CheckLoginRole` or `tabula check-login-role --role app_login --member-of app_reader,app_writer --schemas public` to verify `LOGIN NOINHERIT`, absence of `SUPERUSER`/`BYPASSRLS`/`CREATEROLE`/`CREATEDB`/`REPLICATION`, exact role memberships (including PostgreSQL 16+ per-membership `WITH INHERIT TRUE` grants), zero direct table or column grants, zero owned objects and no `CREATE` privilege on selected schemas.

Set `"AuditSecurityCalls": true` in `inventory.json` (or `InventoryConfig.AuditSecurityCalls = true`) to collect typed call sites for `NewSessionKey`, `Registry.Seal`, `SessionKey.Sign`, keyed runner/Direct factories, `WithoutTransaction` / `MarkTransaction` / `InTransaction`, and both drivers' raw transaction access (`WithinRaw`, `RawSession.Tx`, `ExposeTx`) across import aliases and escaped function or method values. Field targets include direct selections, fields promoted by embedding, and keyed struct literals.
