# Integrating Tabula v0.4

Start here when adding Tabula to an application. The [Go API and CLI reference](REFERENCE.md) lists the public packages, functions, methods, types, constants and command flags in this revision.

## Use the docs that match the build

These pages describe **v0.4**. For a released version, select its Git tag and open the README there. For a development build, open the README at the exact commit you build. Its links to guides and the reference stay on that revision. The changelog records changes between versions. [Migration guide](MIGRATION.md) · [Changelog](../CHANGELOG.md)

From an application checkout, ask Go which copy it actually selected. `Dir` locates its source and docs, including a local workspace or replacement. With a replacement, `Version` can still show the requested tag. Check `Replace.Dir` for the code actually in use. Run the second command as well if the application imports the separate pgxdriver module.

```sh
go list -m -json github.com/0xJean/tabula
go list -m -json github.com/0xJean/tabula/pgxdriver
go list -m -json github.com/0xJean/tabula/oteltabula
```

Read a signature directly from the selected packages, for example with `go doc github.com/0xJean/tabula.Rows.SetWhere`. Build the CLI from the same core revision. From that Tabula checkout, `go install ./cmd/tabula` installs that revision's binary. Run `tabula --help` to list commands and `tabula <command> -h` to inspect its flags.

For a published patch, set `TABULA_VERSION` to its exact tag and run `go get "github.com/0xJean/tabula@$TABULA_VERSION"`. Applications using pgxdriver or oteltabula also pin `github.com/0xJean/tabula/pgxdriver` and `github.com/0xJean/tabula/oteltabula` to that tag.

To try a local checkout from an application that already depends on Tabula, set `TABULA_SRC` to its absolute path. Run this in the application module. The temporary workspace selects the local modules for this run. Include any sibling application modules your build needs in the `go work init` call.

```sh
(
  set -e
  app_dir=$PWD
  work_dir=$(mktemp -d)
  trap 'rm -rf "$work_dir"' EXIT
  (cd "$work_dir" && GOWORK=off go work init "$app_dir")
  export GOWORK="$work_dir/go.work"
  go work edit -replace="github.com/0xJean/tabula=$TABULA_SRC" \
               -replace="github.com/0xJean/tabula/pgxdriver=$TABULA_SRC/pgxdriver" \
               -replace="github.com/0xJean/tabula/oteltabula=$TABULA_SRC/oteltabula"
  go list -m -json github.com/0xJean/tabula
  go test ./...
)
```

## Connect an application

1. Create a registry for your scope type. Declare tables and views at initialization. Register application-specific types, static SQL and any additional privilege requirements there. The compiled handles can then be shared. [Executable descriptor examples](../example_test.go)
2. Choose a session adapter. Set the scope and, if needed, the database role and scope variables when the transaction begins. Bind the handle to that session. A single-scope application can use `NoScopePolicy()` and `struct{}`. [Runnable PostgreSQL example](../examples/multitenant/README.md)
3. Keep the application's role selection, access rules, error mapping and migrations in its own binding. Register the statements and verify them against a disposable installation of the application's schema. [Authorization configurations and worked examples](AUTHORIZATION.md)

## Find a runtime operation

| Task | Start with | Reference or example |
| --- | --- | --- |
| Describe a table, composite key, soft delete, result projection or codec | `TableSpec`, `ViewSpec`, `Define`, `DefineView`, `RegisterType`, `RegisterZero`. Use `Statement` for application SQL and `Requires` or `RequiresFunction` for privileges. | [Core API](REFERENCE.md#tabula) |
| Lock rows, coordinate transactions or call a declared function | `RowsSpec.Lock` adds a lock to each registered data page. Bind an advisory lock or a function plan with `plan.Bind(session)`. | [Examples](COOKBOOK.md#registered-locks-and-functions) |
| Join tables or match a list or pattern | Register `Select(...).From(...)` with `DefineRead`. Declare an `InnerJoin`, `LeftJoin` or `CrossJoin` for each additional occurrence. Typed predicates include `In`, `Any`, `Like` and `ILike`. | [Explicit joins](COOKBOOK.md#explicit-joins) |
| Map PostgreSQL values | `db:"…,json"`, `db:"…,zero"` and `db:"…,array"` select field codecs. Network columns accept `netip.Addr` or `netip.Prefix`. Driver array converters support one-dimensional string, int32, int64, float64 and bool slices. | [Network and array recipes](COOKBOOK.md#13-postgresql-network-addresses-and-arrays) |
| Read, aggregate or lock rows | `Rows.Get`, `GetMany`, `Lock`, `LockMany`, `Find`, `List`, `ListKeyset` (with `KeysetPage.Next`), `Iter`, `Exists`, and package aggregates `Max`, `Min`, `Sum`, `Avg`, `CountBy`. Use `Filter` and `OrderSpec` for runtime values and sort choices, `LockMode` for row locks and `CountMode` for totals. | [Keyset pagination](COOKBOOK.md#6-keyset-pagination-without-offset) · [Streaming and locks](COOKBOOK.md#10-stream-a-worker-batch) |
| Register a typed single-table plan | `RowsFrom`, `DefineRows`, `DefineUpdateMany` and `DefineDeleteMany` compile typed predicates, projections, groups and named alternatives. Enable `RowsSpec.WithExists` for a boolean existence check. | [Registered row plans](COOKBOOK.md#registered-single-table-reads-and-writes) |
| Execute handwritten SQL, composed view queries or embedded SQL files | Use `Rows.SelectIter` for table-shaped results, `View.Statement` + `ViewRows.One`/`Select`/`Iter` for full queries, `View.DefineQuery` (`Select`/`Where`/`Tail`/`CountBase`, `ScopeColumns`/`ScopeByRLS`/`ScopeGlobal`, `QueryParams`) for composed lists/counts/keyset pages, and `MustSQLFiles`/`LoadSQLFiles` for embedded `.sql` files. | [Composed queries](COOKBOOK.md#14-composed-view-queries-and-aggregates) · [Projection example](../example_test.go) |
| Write one row or a batch | `Insert`, `Save`, `SetWhere`, `UpdateWhere`, `SetExpr`, `SoftDelete` and opt-in `Delete`. Use `InsertReturning` or `SaveReturning` for returned columns, `InsertMany` or `UpsertMany` for batches, and `DefineUpsert` for a conflict plan. Use `TableSpec.WriteMode` and `UpdateColumns` for role-specific writes and `RegistryConfig.Deny` for registry-wide limits. | [Batch and atomic updates](COOKBOOK.md#11-batch-upserts-and-atomic-updates) |
| Integrate database/sql or lib/pq | Configure a role-bound `stdsql.NewRunner` with `ScopePolicy` (`ManagedVars`, `ScopeVars`, `ContextVars`, `UnscopedVars`), `RunnerOptions` (`Role`, `ReadOnly`, `Unscoped`, `UnscopedReadRole`, `Timeouts`, `SessionKey`, `ExposeTx`), `WithinShared`, `WithinRetry`, `WithinRaw` or `Direct` pools. Plug in array converters for lib/pq. | [stdsql](REFERENCE.md#stdsql) · [pqdriver](REFERENCE.md#pqdriver) |
| Integrate pgx/v5 | The separate module provides role-bound `Runner`, `WithinShared`, `WithinRetry`, `WithinRaw`, `Session`, `Direct` / `DirectWithConfig`, native array converters and binary `CopyFrom`. Select the matching driver release when pinning modules. | [pgxdriver](REFERENCE.md#pgxdriver) |
| Handle failures and observe queries | Use `ClassifyError` or `SQLState`, inject `ErrorPolicy`, and use `WithinRetry` or `Retry`. Configure logging, redaction, `Tracer`, `Metrics` and `TagStatements` on `RunnerOptions` or `RegistryConfig`, or plug in the OpenTelemetry adapter module. | [oteltabula](REFERENCE.md#oteltabula) · [Logging and telemetry contract](LOGGING.md) |
| Protect session settings and provenance | Seal registries with `NewSessionKey`, `RequireSealed` and `Registry.Seal(key)`, let runners enforce built-in guards for managed variables and timeouts, or apply `GuardNoSessionEscape` / `NewSessionGuard`. Use `WithoutTransaction` only for intentional detached transactions. | [Security model](../SECURITY.md) |

## Verify PostgreSQL

Call the [sqlcheck Go API](REFERENCE.md#sqlcheck) in tests or use the commands below. Database commands take a DSN through `TABULA_DSN` or `--dsn`. Run grant trials and row-behavior tests on a disposable installation. The [authorization guide](AUTHORIZATION.md) owns the JSON formats, examples, and interpretation of findings.

| What to verify | Go entry point | CLI |
| --- | --- | --- |
| Descriptor and view columns against the installed schema | `VerifySchema`, `VerifyViewSchema` | Go API |
| Registered SQL or a committed snapshot, optionally as a role | `RenderAllSnapshot`, `ParseSnapshot`, `VerifyStatements`, `VerifyViewStatements` | [check](REFERENCE.md#tabula-check) |
| Expected table RLS settings | `CheckRLS` | [check-rls](REFERENCE.md#tabula-check-rls) |
| Login role hygiene (`NOINHERIT`, memberships, direct grants, ownership, `CREATE`) | `CheckLoginRole` | [check-login-role](REFERENCE.md#tabula-check-login-role) |
| Handwritten SQL, registered row plans, locks, routine calls, composed queries, aggregates and role bindings | `CollectSQL`, `DefaultSecurityCallTargets`, `SQLInventory.BindRolesWithDefinitions` | [inventory](REFERENCE.md#tabula-inventory) |
| Whether PostgreSQL can plan each role's own statements | `VerifyRoleStatements` | [verify-roles](REFERENCE.md#tabula-verify-roles) |
| Startup readiness of the runner role | `RoleStatements.Readiness` derives requirements from typed statements assigned to the role. `CheckReadiness` checks effective grants and required table RLS flags in one query. | [Startup example](AUTHORIZATION.md#check-a-role-at-startup) |
| Required descriptor privileges or exact direct grants on all configured objects | `CollectPrivilegeNeeds` and `CheckRolePrivileges` for descriptors. `CheckGrantManifest` for missing and extra grants with reconciliation SQL. | [grants](REFERENCE.md#tabula-grants) for the manifest |
| Which grants a tested workload still needs | `MinimizeGrants` | [minimize](REFERENCE.md#tabula-minimize) |
| Policy definitions, FORCE RLS tables, direct grants and SECURITY DEFINER functions and procedures | `ReadCatalog`, `Catalog.Snapshots` | [catalog](REFERENCE.md#tabula-catalog) |
| Whether each policy matches one configured category | `ClassifyPolicies` | [check-policies](REFERENCE.md#tabula-check-policies) |
| Whether a role has policy coverage for the SQL commands it uses | `CheckPolicyCoverage` | [check-coverage](REFERENCE.md#tabula-check-coverage) |
| Which rows a role sees or changes for a given scope | `RunRLSMatrix` | [test-rls](REFERENCE.md#tabula-test-rls) |
| Clone a migrated database for an isolated test, optionally through a selected migration | `postgresfixture.Installation.Through` and `Fixture.Clone` with an application-supplied backend and installer | [postgresfixture](REFERENCE.md#postgresfixture) |
| Install a test template before acquiring a login that needs migration-created roles | `postgresfixture.Fixture.Prepare` | [Cold-start example](AUTHORIZATION.md#process-login-roles) |
| Give a test process its own login and clean up abandoned logins | `postgresfixture.Fixture.AcquireLoginRole`, `LoginRole.Release` and `Fixture.PruneLoginRoles` | [Fixture role lifecycle](AUTHORIZATION.md#process-login-roles) |
| Reclaim a test process's idle databases after a crash | `Fixture.CloneWithLease` and `Fixture.PruneClones` | [Clone ownership](DESIGN-v0.3.9.md) |
| Drop cached test templates that are no longer used | `postgresfixture.Fixture.Prune` | [postgresfixture](REFERENCE.md#postgresfixture) |

A successful plan checks that PostgreSQL can prepare and explain the statement as the role. To verify row visibility or a denial at execution time, give the behavior matrix representative rows and expected outcomes.
