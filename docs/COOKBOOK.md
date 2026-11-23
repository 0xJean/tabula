# Tabula Cookbook

Practical recipes showing how applications bind and use Tabula without an ORM.

## 1. Setup with `stdsql` and a tenant/RLS scope plug-in

```go
type TenantID string

var reg = tabula.NewRegistry(tabula.RegistryConfig[TenantID]{
    TenantID:    func(t TenantID) any { return string(t) },
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

For single-tenant applications, pass `stdsql.NoScopePolicy()` with scope type `struct{}`.

## 2. Single-table CRUD with optimistic locking and `once` protection

```go
type Account struct {
    ID           int64      `db:"id,key,serial"`
    Tenant       TenantID   `db:"tenant_id,tenant"`
    Code         string     `db:"code"`
    PasswordHash string     `db:"password_hash,secret"`
    CreatedBy    string     `db:"created_by,once"`
    Version      int        `db:"version,version"`
    DeletedAt    *time.Time `db:"deleted_at,deleted"`
}

var accounts = tabula.Define(reg, tabula.Table[Account, int64, TenantID]{
    Name:  "public.accounts",
    Order: "id ASC",
})
```

Inside `runner.Within(ctx, tenant, ...)`:

```go
r := accounts.Bind(sess)
row, err := r.Lock(ctx, tenant, id) // FOR UPDATE; remembers once columns
row.Code = "updated"
row.Version++
err = r.Save(ctx, *row)             // checks version CAS and once columns
```

## 3. Conditional state transitions (`UpdateWhere` / `SetWhere`)

```go
row.Status = "settled"
matched, err := r.SetWhere(ctx, row, []string{"status"}, "status = ?", "pending")
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

`DefineUpsert` refuses `key`, `tenant`, and `once` columns in `Update` at start-up and
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

var orderView = tabula.DefineView(reg, tabula.View[OrderSummary, TenantID]{
    Name: "public.order_summaries",
})

var listOrdersSQL = orderView.Statement("list",
    "SELECT "+orderView.SelectAs(map[string]string{"o_": "o", "c_": "c"})+
        " FROM public.orders o JOIN public.customers c ON c.id = o.customer_id WHERE o.tenant_id = $1",
)
```

## 6. Keyset pagination without `OFFSET`

```go
page, err := r.ListKeyset(ctx, tenant, func(f *tabula.Filter) {
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
err := stdsql.Retry(ctx, stdsql.RetryConfig{MaxAttempts: 3}, func(ctx context.Context) error {
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
n, err := pgxdriver.CopyFrom(ctx, pgxSession, accounts, batch)
```

## 9. CI verification with `tabula check`

Commit the rendered SQL snapshot from your tests (`sqlcheck.RenderSnapshot(reg.Defined())`),
then run in CI against a migrated database:

```sh
go run github.com/0xJean/tabula/cmd/tabula@v0.3.0 check \
  --dsn "$POSTGRES_DSN" \
  --snapshot test/sqlprepare/testdata/defined-statements.sql \
  --role app_user

# Also audit Row-Level Security (ENABLE + FORCE + RESTRICTIVE):
go run github.com/0xJean/tabula/cmd/tabula@v0.3.0 check-rls \
  --dsn "$POSTGRES_DSN" \
  --schema public \
  --force \
  --restrictive
```
