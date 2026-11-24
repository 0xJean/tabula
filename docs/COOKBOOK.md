# Tabula Cookbook

Practical recipes showing how applications bind and use Tabula without an ORM.

## 1. Setup with `stdsql` and a row-scope / RLS plug-in

```go
type TenantID string

var reg = tabula.NewRegistry(tabula.RegistryConfig[TenantID]{
    RequireSealed: true,
    ScopeID:       func(t TenantID) any { return string(t) },
    ScopeLog:      func(t TenantID) string { return string(t) },
    ArrayArg:      pqdriver.Array,
    ArrayColumn:   pqdriver.ArrayColumn,
})

key := tabula.NewSessionKey()
runner, err := stdsql.NewRunner(db, stdsql.ScopePolicy[TenantID]{
    ManagedVars: []string{"app.current_tenant"},
    ScopeVars: func(tenant TenantID) map[string]string {
        return map[string]string{"app.current_tenant": string(tenant)}
    },
    ScopeLog: func(t TenantID) string { return string(t) },
}, stdsql.RunnerOptions{
    Role:        "app_user",
    RequireRole: true,
    SessionKey:  key,
    Timeouts: stdsql.Timeouts{
        Lock:              time.Second,
        Statement:         5 * time.Second,
        IdleInTransaction: 10 * time.Second,
        Transaction:       30 * time.Second,
    },
    Log: tabula.LogConfig{
        Logger:        logger,
        Level:         tabula.LogSlow,
        SlowThreshold: 200 * time.Millisecond,
        Values:        tabula.ValuesNever,
    },
})
if err != nil {
    return err
}
if err := reg.Seal(key); err != nil {
    return err
}
```

For single-scope applications, pass `stdsql.NoScopePolicy()` and `stdsql.RunnerOptions{}` with scope type `struct{}`.

## 2. Single-table CRUD with optimistic locking and `once` protection

```go
type Record struct {
    ID           int64      `db:"id,key,serial"`
    Tenant       TenantID   `db:"tenant_id,scope"`
    Code         string     `db:"code"`
    Status       string     `db:"status"`
    CreatedAt    time.Time  `db:"created_at,once"`
    PasswordHash string     `db:"password_hash,secret"`
    CreatedBy    string     `db:"created_by,once"`
    Version      int        `db:"version,version"`
    DeletedAt    *time.Time `db:"deleted_at,deleted"`
}

var records = tabula.Define(reg, tabula.TableSpec[Record, int64]{
    Name:  "public.records",
    Order: "id ASC",
})
```

Inside `runner.Within(ctx, tenant, ...)`:

```go
r := records.Bind(sess)
row, err := r.Lock(ctx, id) // FOR UPDATE; remembers once columns
if err != nil { return err }
if row == nil { return ErrRecordMissing } // application error
row.Code = "updated"
row.Version++
err = r.Save(ctx, *row) // checks version CAS and once columns
```

## 3. Conditional state transitions (`UpdateWhere` / `SetWhere`)

```go
change := Record{ID: id, Tenant: sess.Scope(), Status: "published"}
matched, err := r.SetWhere(ctx, change, []string{"status"}, "status = ?", "draft")
if err != nil {
    return err
}
if !matched {
    // zero rows is not an error — handle concurrent transition cleanly
}
```

## 4. Controlled Upsert (`DefineUpsert` + `Upsert`)

```go
var upsertConfig = configs.DefineUpsert("upsert_key", tabula.UpsertSpec{
    Conflict:  []string{"tenant_id", "config_key"},
    Update:    []string{"config_value", "updated_at"},
    Returning: []string{"id", "updated_at"},
})

changed, err := configs.Bind(sess).Upsert(ctx, upsertConfig, &row)
```

`DefineUpsert` refuses `key`, `scope`, and `once` columns in `Update` at start-up and
registers the SQL into `Definition.Statements` so `sqlcheck` verifies it against PostgreSQL.

## 5. JOIN projection with nested `View` prefixes

```go
type OrderPart struct {
    ID   int64  `db:"id"`
    Code string `db:"code"`
}
type CustomerPart struct {
    Name string `db:"name,zero"`
}
type OrderSummary struct {
    Order    OrderPart    `db:"o_"`
    Customer CustomerPart `db:"c_"`
}

var orderView = tabula.DefineView(reg, tabula.ViewSpec[OrderSummary]{
    Name: "public.order_summaries",
})

var listOrdersSQL = orderView.Statement("list",
    "SELECT "+orderView.SelectAs(map[string]string{"o_": "o", "c_": "c"})+
        " FROM public.orders o JOIN public.customers c ON c.id = o.customer_id WHERE o.tenant_id = $1",
)
```

## 6. Keyset pagination without `OFFSET`

```go
page, err := r.ListKeyset(ctx, func(f *tabula.Filter) {
    f.AndIf(status != "", "status = ?", status)
    f.Range("created_at", fromTime, toTime)
}, tabula.KeysetListing{
    SortColumn: "created_at",
    Direction:  "DESC",
    After:      cursor, // nil for page 1; []any{last.CreatedAt, last.ID} for next page
    Limit:      20,
})
```

## 7. Serializable transactions with explicit retry

```go
err := runner.WithinRetryWith(ctx, tenant, tabula.RetryConfig{MaxAttempts: 3}, stdsql.RunOptions[TenantID]{
    TxOptions: &sql.TxOptions{Isolation: sql.LevelSerializable},
}, func(ctx context.Context, s *stdsql.Session[TenantID]) error {
    // Callback must be idempotent apart from database writes.
    return nil
})
```

## 8. High-throughput bulk inserts with `pgxdriver.CopyFrom`

For thousands of rows in one batch, `pgxdriver.CopyFrom` streams directly over PostgreSQL's
binary `COPY` protocol while reusing your `Table` descriptor and codecs:

```go
n, err := pgxdriver.CopyFrom(ctx, pgxSession, records, batch)
```

## 9. CI verification with `tabula check`

Commit the rendered SQL snapshot from your tests (`sqlcheck.RenderSnapshot(reg.Defined())`),
then run the CLI built from the same Tabula revision against a migrated test database:

```sh
tabula check \
  --dsn "$POSTGRES_DSN" \
  --snapshot test/sqlprepare/testdata/defined-statements.sql \
  --role app_user

# Also audit Row-Level Security (ENABLE + FORCE + RESTRICTIVE):
tabula check-rls \
  --dsn "$POSTGRES_DSN" \
  --schema public \
  --force \
  --restrictive
```

## 10. Stream a worker batch

```go
for job, err := range jobs.Bind(sess).Iter(ctx, nil, tabula.Listing{
    Order: "id", Size: 50, Lock: tabula.LockSkipLocked,
}) {
    if err != nil { return err }
    ids = append(ids, job.ID)
}
// The cursor is closed. Process the claimed IDs in the same transaction.
```

Starting a range executes one query in the bound session. Each yielded row comes from that cursor. A break releases it. Finish the range or break before issuing another statement on the same transaction. Both drivers return `tabula.ErrSessionBusy` while the cursor is open, including when another handle from `WithinShared` issues the statement. PostgreSQL retains row locks until the transaction ends. Streaming a locked table with `once` columns retains a snapshot per locked row. Unlocked exports decode one row at a time. For hand-written SQL over the table columns use `rows.SelectIter(ctx, query, args...)`. For a projection, use `view.Bind(sess).Iter(ctx, query, args...)`.

When pulling rows with `next, stop := iter.Pull2(seq)`, place `defer stop()` in the `Within` or `Savepoint` callback that consumes the rows, immediately after creating the iterator. Returning with a partially consumed iterator still open can leave transaction or savepoint cleanup waiting indefinitely.

## 11. Batch upserts and atomic updates

```go
changed, err := configs.Bind(sess).UpsertMany(ctx, upsertConfig, batch)
if err != nil { return err }
// changed counts the rows inserted or updated.
err = jobs.Bind(sess).SetExpr(ctx, jobID,
    tabula.Add("attempts", 1), tabula.SetNow("updated_at"))
```

`UpsertMany` reports inserted and updated rows. A conflict skipped by `DO NOTHING` or a plan predicate contributes zero. The batch omits a single-row plan's `RETURNING` list and leaves its input untouched. Each chunk fits the PostgreSQL bind limit. Propagate an error out of the runner callback to roll back earlier chunks. Ensure a `DO UPDATE` chunk contains each conflict key at most once, as required by PostgreSQL.

`SetExpr` checks the key, session scope, live-row predicate and affected row count. `Add` takes a bound numeric value. `SetNow` uses the database transaction timestamp. Both operate on declared, mutable payload columns.

## 12. Single-statement reads on a pool

```go
pool, err := stdsql.Direct("postgres", readURL, stdsql.NoScopePolicy())
if err != nil { return err }
pool.SetMaxOpenConns(16)
pool.SetMaxIdleConns(4)
defer pool.Close()

row, err := settings.Bind(pool.Session(struct{}{})).Get(ctx, key)
```

Create the pool at startup and reuse it across requests. Both `stdsql.Direct` and `pgxdriver.Direct` set `default_transaction_read_only=on` when physical connections open. Tune pool limits via `stdsql.DirectPool`'s `SetMaxOpenConns`/`SetMaxIdleConns`/`SetConnMaxLifetime`/`SetConnMaxIdleTime` (and inspect `Stats()`), or pass `pgxdriver.DirectOption` callbacks to `pgxdriver.Direct` (and inspect `Stat()`); `pgxdriver.Direct` always re-enforces `default_transaction_read_only=on` after applying options. A `Get` or `View.One` uses one query round trip on a warm connection. A `List` with `CountExact` still issues a count and a page query, each with its own snapshot.

Use a read-role connection URL and application-owned SQL. Both Direct adapters apply `tabula.GuardReadOnly` before the application's `GuardQuery`. It accepts one `SELECT` or `WITH` statement at a time. PostgreSQL enforces read-only access inside the query. Use `WithinReadOnly` for transaction-local roles, scope GUCs or a shared transaction snapshot with an appropriate isolation level (`WithinReadOnly` sessions enforce `tabula.ErrReadOnly` consistently with `Direct`). Direct rejects a policy declaring managed transaction-local variables. For sealed registries, pass `stdsql.DirectSessionKey(key)` or `pgxdriver.DirectWithConfig(..., DirectConfig{SessionKey: key})`.

## 13. PostgreSQL network addresses and arrays

Use `netip.Addr` for a host address and `netip.Prefix` for CIDR networks or inet values with a subnet mask. An SQL NULL maps to an invalid address or prefix, or to nil for pointer fields. Scanning a subnet into a host address returns an error so the prefix length is preserved.

Set `RegistryConfig.ArrayColumn` to `pqdriver.ArrayColumn` or `pgxdriver.ArrayColumn` for one-dimensional string, int32, int64, float64 and bool slices, including named element types. A nil slice represents SQL NULL and a non-nil empty slice represents an empty PostgreSQL array.

## 14. Composed view queries and aggregates

```go
// Single-table aggregates reuse declared columns and registry codecs.
peak, present, err := tabula.Max[int64](ctx, rows, "rank", func(f *tabula.Filter) {
    f.And("visible = ?", true)
})
counts, err := tabula.CountBy[string](ctx, rows, "category", nil)
exists, err := rows.Exists(ctx, func(f *tabula.Filter) { f.And("pinned = ?", true) })

// View-owned composed query with Select/Where split, local $1 filters and keyset paging.
var listNotes = noteView.DefineQuery("notes.search", tabula.QuerySpec{
    Select: "SELECT n.id, n.rank, coalesce(l.caption, '') AS caption FROM public.notes n LEFT JOIN public.labels l ON l.id = n.label_id AND l.tenant_id = n.tenant_id",
    Where:  "n.deleted_at IS NULL",
    Scope:  tabula.ScopeColumns("n.tenant_id"),
    Filters: []tabula.QueryFilter{
        {Name: "q", SQL: "n.title ILIKE $1 OR n.code ILIKE $1", Secret: true},
    },
    Order: tabula.QueryOrderSpec{
        Default:          "rank",
        DefaultDirection: tabula.SortDesc,
        Sorts:            []tabula.QuerySort{{Key: "rank", Expr: "n.rank", Column: "rank"}},
        TieBreaker:       []tabula.QuerySortTerm{{Expr: "n.id", Column: "id"}},
    },
    MaxLimit: 100,
    Keyset:   true,
})

// Zero-value QueryParams is directly usable without a constructor.
var params tabula.QueryParams
if search != "" {
    params.With("q", "%"+search+"%")
}
page, err := listNotes.Bind(sess).ListKeyset(ctx, &params, tabula.QueryKeyset{Limit: 20, After: cursor})
// page.Next is automatically populated when page.HasMore is true.
```

Declare sort and tie-breaker expressions without bind parameters. An ordering that depends on a base argument can use a projected column computed in `Select`. For keyset pagination, expose that column from a subquery so the cursor predicate in `WHERE` can reference it.

With a fixed keyset order, set the same `Direction` on the primary sort and each tie-breaker. Both `ORDER BY` and the `After` comparison use that direction even if the request asks for the opposite order. Leave every term at `SortDefault` to let the caller choose.

## Audit handwritten SQL and login roles before narrowing grants

Run these commands in the application repository using the application’s inventory, workload, scope and expected grant files. Set `TABULA_DSN` to a disposable PostgreSQL installation.

```sh
tabula check-login-role --role app_login --member-of app_reader,app_writer --schemas public
tabula inventory --config inventory.json --bindings bindings.json --snapshot statements.sql > workload.json
tabula verify-roles --config workload.json
tabula grants --config privileges.json
tabula minimize --config trials.json
tabula check-coverage --config workload.json
tabula test-rls --config behavior.json
```

Generate `statements.sql` from the tables and views used by the assigned roles. A missing descriptor or aggregate makes `inventory` exit with the affected site ID. Review each unregistered dynamic site and each extra grant before changing the application's grant declarations. [Full JSON examples and interpretation](AUTHORIZATION.md)
