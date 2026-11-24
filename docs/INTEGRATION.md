# Integrating Tabula v0.3

Start here when adding Tabula to an application. The [Go API and CLI reference](REFERENCE.md) lists the public packages, functions, methods, types, constants and command flags in this revision.

## Use the docs that match the build

These pages describe **v0.3**. For a released version, select its Git tag and open the README there. For a development build, open the README at the exact commit you build. Its links to guides and the reference stay on that revision. The changelog records changes between versions. [Migration guide](MIGRATION.md) · [Changelog](../CHANGELOG.md)

From an application checkout, ask Go which copy it actually selected. `Dir` locates its source and docs, including a local workspace or replacement. With a replacement, `Version` can still show the requested tag. Check `Replace.Dir` for the code actually in use. Run the second command as well if the application imports the separate pgxdriver module.

```sh
go list -m -json github.com/0xJean/tabula
go list -m -json github.com/0xJean/tabula/pgxdriver
```

Read a signature directly from the selected packages, for example with `go doc github.com/0xJean/tabula.Rows.SetWhere`. Build the CLI from the same core revision. From the v0.3 Tabula checkout, `go install ./cmd/tabula` installs that revision's binary. Run `tabula --help` to list commands and `tabula <command> -h` to inspect its flags.

For a published patch, set `TABULA_VERSION` to its exact tag and run `go get "github.com/0xJean/tabula@$TABULA_VERSION"`. Applications using pgxdriver also pin `github.com/0xJean/tabula/pgxdriver` to that tag.

To try a local v0.3 checkout from an application that already depends on Tabula, set `TABULA_SRC` to its absolute path. Run this in the application module. The temporary workspace selects the local modules for this run. Include any sibling application modules your build needs in the `go work init` call.

```sh
(
  set -e
  app_dir=$PWD
  work_dir=$(mktemp -d)
  trap 'rm -rf "$work_dir"' EXIT
  (cd "$work_dir" && GOWORK=off go work init "$app_dir")
  export GOWORK="$work_dir/go.work"
  go work edit -replace="github.com/0xJean/tabula=$TABULA_SRC" \
               -replace="github.com/0xJean/tabula/pgxdriver=$TABULA_SRC/pgxdriver"
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
| Map PostgreSQL values | `db:"…,json"`, `db:"…,zero"` and `db:"…,array"` select field codecs. Network columns accept `netip.Addr` or `netip.Prefix`. Driver array converters support one-dimensional string, int32, int64, float64 and bool slices. | [Network and array recipes](COOKBOOK.md#13-postgresql-network-addresses-and-arrays) |
| Read or lock rows | `Rows.Get`, `GetMany`, `Lock`, `LockMany`, `Find`, `List`, `ListKeyset`, `Iter`. Use `Filter` and `OrderSpec` for runtime values and sort choices, `LockMode` for row locks and `CountMode` for totals. | [Keyset pagination](COOKBOOK.md#6-keyset-pagination-without-offset) · [Streaming and locks](COOKBOOK.md#10-stream-a-worker-batch) |
| Execute handwritten SQL or stream a join | Use `Rows.SelectIter` for table-shaped results, or declare a projection and use `ViewRows.One`, `Select` or `Iter`. | [Projection example](../example_test.go) |
| Write one row or a batch | `Insert`, `Save`, `SetWhere`, `UpdateWhere`, `SetExpr`, `SoftDelete` and opt-in `Delete`. Use `InsertReturning` or `SaveReturning` for returned columns, `InsertMany` or `UpsertMany` for batches, and `DefineUpsert` for a conflict plan. | [Batch and atomic updates](COOKBOOK.md#11-batch-upserts-and-atomic-updates) |
| Integrate database/sql or lib/pq | Wrap a transaction with `NewSession` or let a `Runner` handle transactions, isolation, read-only work and savepoints. `Direct` supports reads on a pool. Plug in array converters for lib/pq. | [stdsql](REFERENCE.md#stdsql) · [pqdriver](REFERENCE.md#pqdriver) |
| Integrate pgx/v5 | The separate module provides sessions, runners, read-only pools, native array converters and binary `CopyFrom`. Select the matching driver release when pinning modules. | [pgxdriver](REFERENCE.md#pgxdriver) |
| Handle failures and observe queries | Use `ClassifyError` or `SQLState`, inject `ErrorPolicy`, and wrap a retryable transaction with `Retry`. Configure logging, redaction, `Tracer` and `Metrics` on the registry. | [Logging and telemetry contract](LOGGING.md) |
| Protect session settings | Apply `GuardNoSessionEscape` or configure `NewSessionGuard` on the scope policy. PostgreSQL roles, scope variables and exceptions are application choices. | [Security model](../SECURITY.md) |

## Verify PostgreSQL

Call the [sqlcheck Go API](REFERENCE.md#sqlcheck) in tests or use the commands below. Database commands take a DSN through `TABULA_DSN` or `--dsn`. Run grant trials and row-behavior tests on a disposable installation. The [authorization guide](AUTHORIZATION.md) owns the JSON formats, examples, and interpretation of findings.

| What to verify | Go entry point | CLI |
| --- | --- | --- |
| Descriptor and view columns against the installed schema | `VerifySchema`, `VerifyViewSchema` | Go API |
| Registered SQL or a committed snapshot, optionally as a role | `RenderAllSnapshot`, `ParseSnapshot`, `VerifyStatements`, `VerifyViewStatements` | [check](REFERENCE.md#tabula-check) |
| Expected table RLS settings | `CheckRLS` | [check-rls](REFERENCE.md#tabula-check-rls) |
| Handwritten SQL, dynamic calls and reviewed exceptions in Go source | `CollectSQL`, `SQLInventory.BindRoles` | [inventory](REFERENCE.md#tabula-inventory) |
| Whether PostgreSQL can plan each role's own statements | `VerifyRoleStatements` | [verify-roles](REFERENCE.md#tabula-verify-roles) |
| Required descriptor privileges or exact direct grants on all configured objects | `CollectPrivilegeNeeds` and `CheckRolePrivileges` for descriptors. `CheckGrantManifest` for missing and extra grants with reconciliation SQL. | [grants](REFERENCE.md#tabula-grants) for the manifest |
| Which grants a tested workload still needs | `MinimizeGrants` | [minimize](REFERENCE.md#tabula-minimize) |
| Policy definitions, FORCE RLS tables, grants and SECURITY DEFINER functions | `ReadCatalog`, `Catalog.Snapshots` | [catalog](REFERENCE.md#tabula-catalog) |
| Whether each policy matches one configured category | `ClassifyPolicies` | [check-policies](REFERENCE.md#tabula-check-policies) |
| Whether a role has policy coverage for the SQL commands it uses | `CheckPolicyCoverage` | [check-coverage](REFERENCE.md#tabula-check-coverage) |
| Which rows a role sees or changes for a given scope | `RunRLSMatrix` | [test-rls](REFERENCE.md#tabula-test-rls) |
| Clone a migrated database for an isolated test, optionally through a selected migration | `postgresfixture.Installation.Through` and `Fixture.Clone` with an application-supplied backend and installer | [postgresfixture](REFERENCE.md#postgresfixture) |

A successful plan checks that PostgreSQL can prepare and explain the statement as the role. To verify row visibility or a denial at execution time, give the behavior matrix representative rows and expected outcomes.
