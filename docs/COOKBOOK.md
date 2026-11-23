# Tabula Cookbook

Practical recipes showing how applications bind and use Tabula without an ORM.

## 1. Setup with `stdsql` and a row-scope / RLS plug-in

```go
type TenantID string

var reg = tabula.NewRegistry(tabula.RegistryConfig[TenantID]{
    ScopeID:     func(t TenantID) any { return string(t) },
    ScopeLog:    func(t TenantID) string { return string(t) },
    ArrayArg:    pqdriver.Array,
    ArrayColumn: pqdriver.ArrayColumn,
    Log: tabula.LogConfig{
        Logger:        logger,
        Level:         tabula.LogSlow,
        SlowThreshold: 200 * time.Millisecond,
        Values:        tabula.ValuesNever,
    },
})

runner := stdsql.NewRunner(db, stdsql.ScopePolicy[TenantID]{
    SetupTx: func(ctx context.Context, tx *sql.Tx, tenant TenantID) error {
        if err := stdsql.SetLocalRole(ctx, tx, "app_user"); err != nil {
            return err
        }
        return stdsql.SetLocalConfig(ctx, tx, "app.current_tenant", string(tenant))
    },
    GuardQuery: tabula.GuardNoSessionEscape,
})
```

For single-scope applications, pass `stdsql.NoScopePolicy()` with scope type `struct{}`.

## 2. Single-table CRUD with optimistic locking and `once` protection

```go
type Record struct {
    ID           int64      `db:"id,key,serial"`
    Tenant       TenantID   `db:"tenant_id,scope"`
    Code         string     `db:"code"`
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
row.Status = "published"
matched, err := r.SetWhere(ctx, row, []string{"status"}, "status = ?", "draft")
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
err := tabula.Retry(ctx, tabula.RetryConfig{MaxAttempts: 3}, func(ctx context.Context) error {
    return runner.WithinIsolation(ctx, tenant, sql.LevelSerializable, func(ctx context.Context, s *stdsql.Session[TenantID]) error {
        // ...
        return nil
    })
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
then run in CI against a migrated database:

```sh
go run ./cmd/tabula check \
  --dsn "$POSTGRES_DSN" \
  --snapshot test/sqlprepare/testdata/defined-statements.sql \
  --role app_user

# Also audit Row-Level Security (ENABLE + FORCE + RESTRICTIVE):
go run ./cmd/tabula check-rls \
  --dsn "$POSTGRES_DSN" \
  --schema public \
  --force \
  --restrictive
```

## Stream a worker batch

```go
for job, err := range jobs.Bind(sess).Iter(ctx, nil, tabula.Listing{
    Order: "id", Size: 50, Lock: tabula.LockSkipLocked,
}) {
    if err != nil { return err }
    ids = append(ids, job.ID)
}
// The cursor is closed. Process the claimed IDs in the same transaction.
```

Each iteration executes the query in the bound session. A break releases the cursor. Finish the range or break before issuing another statement on the same transaction. PostgreSQL retains row locks until the transaction ends. Streaming a locked table with `once` columns retains a snapshot per locked row. Unlocked exports decode one row at a time. For hand-written SQL over the table columns use `rows.SelectIter(ctx, query, args...)`. For a projection, use `view.Bind(sess).Iter(ctx, query, args...)`.

## Batch upserts and atomic updates

```go
changed, err := configs.Bind(sess).UpsertMany(ctx, upsertConfig, batch)
if err != nil { return err }
// changed counts the rows inserted or updated.
err = jobs.Bind(sess).SetExpr(ctx, jobID,
    tabula.Add("attempts", 1), tabula.SetNow("updated_at"))
```

`UpsertMany` reports inserted and updated rows. A conflict skipped by `DO NOTHING` or a plan predicate contributes zero. The batch omits a single-row plan's `RETURNING` list and leaves its input untouched. Each chunk fits the PostgreSQL bind limit. Propagate an error out of the runner callback to roll back earlier chunks. Ensure a `DO UPDATE` chunk contains each conflict key at most once, as required by PostgreSQL.

`SetExpr` checks the key, session scope, live-row predicate and affected row count. `Add` takes a bound numeric value. `SetNow` uses the database transaction timestamp. Both operate on declared, mutable payload columns.

## Single-statement reads on a pool

```go
pool, err := stdsql.Direct("postgres", readURL, stdsql.NoScopePolicy())
if err != nil { return err }
defer pool.Close()

row, err := settings.Bind(pool.Session(struct{}{})).Get(ctx, key)
```

Create the pool at startup and reuse it across requests. Both `stdsql.Direct` and `pgxdriver.Direct` set `default_transaction_read_only=on` when physical connections open. A Get or View.One uses one query round trip on a warm connection. A List with CountExact still issues a count and a page query, each with its own snapshot.

Use a read-role connection URL and application-owned SQL. Both Direct adapters apply `tabula.GuardReadOnly` before the application's `GuardQuery`. It accepts one SELECT or WITH statement at a time. PostgreSQL enforces read-only access inside the query. Use `WithinReadOnly` for transaction-local roles, tenant GUCs or a shared transaction snapshot with an appropriate isolation level. Direct rejects a policy with `SetupTx`.

## PostgreSQL network addresses and arrays

Use `netip.Addr` for a host address and `netip.Prefix` for CIDR networks or inet values with a subnet mask. An SQL NULL maps to an invalid address or prefix, or to nil for pointer fields. Scanning a subnet into a host address returns an error so the prefix length is preserved.

Set `RegistryConfig.ArrayColumn` to `pqdriver.ArrayColumn` or `pgxdriver.ArrayColumn` for one-dimensional string, int32, int64, float64 and bool slices, including named element types. A nil slice represents SQL NULL and a non-nil empty slice represents an empty PostgreSQL array.
