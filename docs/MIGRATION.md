# Migrating to the development API

The current checkout contains the next minor release. The published v0.2.0 tag has the previous signatures.

## From Tabula v0.2 or an earlier development checkout

| Previous call or declaration | Current form |
| --- | --- |
| `Define(reg, Table[Order, int64, Tenant]{…})` | `Define(reg, TableSpec[Order, int64]{…})` |
| `DefineView(reg, View[Summary, Tenant]{…})` | `DefineView(reg, ViewSpec[Summary]{…})` |
| `tbl.Name`, `tbl.ReadOnly`, `tbl.AllowDelete` | `tbl.Name()`, `tbl.ReadOnly()`, `tbl.AllowsDelete()` |
| `rows.Get(ctx, scope, id)`, `rows.Delete(ctx, scope, id)` | `rows.Get(ctx, id)`, `rows.Delete(ctx, id)` |
| `rows.List(ctx, scope, where, page)` | `rows.List(ctx, where, page)` |
| `view.One(ctx, scope, sql, args…)` | `view.One(ctx, sql, args…)` |
| `session.Exec(ctx, scope, sql, args…)` | `session.Exec(ctx, sql, args…)` |
| `Listing{Lock: true}`, `KeysetListing{Lock: true}` | `Listing{Lock: LockUpdate}`, `KeysetListing{Lock: LockUpdate}` |
| `stdsql.Retry`, `stdsql.RetryConfig`, `stdsql.IsRetryable` | `tabula.Retry`, `tabula.RetryConfig`, `tabula.IsRetryable` |
| `pqdriver.IsUniqueViolation`, `pgxdriver.SQLState` | `tabula.IsUniqueViolation`, `tabula.SQLState`. The other SQLSTATE helpers move in the same way. |

Declare the name, default order, escape columns, soft-delete policy and write permissions in the spec. The registry supplies the scope type. The compiled table or view keeps its own bindings. `Definition()` returns a snapshot. `Statement`, `Requires`, `RequiresFunction` and `DefineUpsert` register additional SQL and privilege metadata during application initialization.

### Move tenant selection to the transaction boundary

Open a runner with the authenticated tenant. Derived reads and deletes use `Session.Tenant()`. Inserts, updates, upserts and pgx COPY check the row tenant against that scope before sending SQL. For structured scopes, `RegistryConfig.TenantID` should return a stable SQL identity. A mismatch returns `ErrScopeMismatch`. A platform session can address rows across tenants.

For a tenant-visible pagination total, use `CountExact` (the default). `CountEstimate` reads PostgreSQL's table-wide row estimate and includes other tenants. `CountNone` skips the count.

If a request supplies a second, independently obtained tenant ID, preserve that authorization check in the application binding. For example, when both IDs are comparable strings:

```go
if requestTenant != sess.Tenant() {
    return ErrTenantMismatch // application error
}
row, err := notes.Bind(sess).Get(ctx, id)
```

Custom session adapters remove the scope parameter from `Exec`, `Scan`, `Returning` and `Each`. Keep `Tenant()` and `Platform()` stable for the lifetime of a binding. An `Each` implementation must stop on a callback error, close the cursor and return the error. Iterators use this contract to release resources on `break`.

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
