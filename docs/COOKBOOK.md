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
    ManagedVars: []string{"demo.tenant"},
    ScopeVars: func(tenant TenantID) map[string]string {
        return map[string]string{"demo.tenant": string(tenant)}
    },
    ScopeLog: func(t TenantID) string { return string(t) },
}, stdsql.RunnerOptions{
    Role:        "scope_user",
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

A runner that accepts only application reads and writes can set `RunnerOptions.Statements`. For example, allow `select` on `Scan` and `Each`, and `insert`, `update` and `delete` on `Exec` and `Returning`. Set `RequireReturning` when a write that scans a row must explicitly return one. The runner checks each statement locally, including descriptor-generated SQL. [Configuration example](MIGRATION.md#from-v035-to-v036)

## 2. Single-table CRUD with optimistic locking and `once` protection

```go
type Entry struct {
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

var entries = tabula.Define(reg, tabula.TableSpec[Entry, int64]{
    Name:  "public.entries",
    Order: "id ASC",
})
```

Inside `runner.Within(ctx, tenant, ...)`:

```go
r := entries.Bind(sess)
row, err := r.Lock(ctx, id) // FOR UPDATE; remembers once columns
if err != nil { return err }
if row == nil { return ErrRecordMissing } // application error
row.Code = "updated"
row.Version++
err = r.Save(ctx, *row) // checks version CAS and once columns
```

### Give an update role access to two columns

```go
var editable = tabula.Define(reg, tabula.TableSpec[Entry, int64]{
    Name:          "public.entries",
    WriteMode:     tabula.WriteUpdateOnly,
    UpdateColumns: []string{"code", "version"},
})
```

A locked entry can be saved with a new code and an incremented version. The generated `UPDATE` assigns those two columns. The role needs `SELECT` on the declared columns for the lock and `UPDATE (code, version)` for the save. [Table write modes](AUTHORIZATION.md#table-write-modes)

## 3. Conditional state transitions (`UpdateWhere` / `SetWhere`)

```go
change := Entry{ID: id, Tenant: sess.Scope(), Status: "published"}
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
type JobPart struct {
    ID   int64  `db:"id"`
    Code string `db:"code"`
}
type GroupPart struct {
    Name string `db:"name,zero"`
}
type JobSummary struct {
    Job    JobPart    `db:"j_"`
    Group GroupPart `db:"g_"`
}

var jobView = tabula.DefineView(reg, tabula.ViewSpec[JobSummary]{
    Name: "public.job_summaries",
})

var listJobsSQL = jobView.Statement("list",
    "SELECT "+jobView.SelectAs(map[string]string{"j_": "j", "g_": "g"})+
        " FROM public.jobs j JOIN public.groups g ON g.id = j.group_id WHERE j.tenant_id = $1",
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
n, err := pgxdriver.CopyFrom(ctx, pgxSession, entries, batch)
```

## 9. CI verification with `tabula check`

Commit the rendered SQL snapshot from your tests (`sqlcheck.RenderSnapshot(reg.Defined())`),
then run the CLI built from the same Tabula revision against a migrated test database:

```sh
tabula check \
  --dsn "$POSTGRES_DSN" \
  --snapshot test/sqlprepare/testdata/defined-statements.sql \
  --role scope_user

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

When pulling rows with `next, stop := iter.Pull2(seq)`, place `defer stop()` in the `Within` or `Savepoint` callback that consumes the rows, immediately after creating the iterator. Both drivers close a partially consumed database cursor on transaction or savepoint exit. With a live context, a later pull reports `ErrSessionClosed`. A canceled context keeps its cancellation error. Calling `stop` also releases the Go iterator and lets you reuse the transaction earlier.

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
    Select: "SELECT n.id, n.rank, coalesce(g.caption, '') AS caption FROM public.notes n LEFT JOIN public.groups g ON g.id = n.group_id AND g.tenant_id = n.tenant_id",
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
    MaxOffset: 1000,
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

Set `MaxOffset` to the largest offset this query should accept. Larger offsets return an error before either the count or page query runs. A zero cap allows any nonnegative offset.

Declare sort and tie-breaker expressions without bind parameters. An ordering that depends on a base argument can use a projected column computed in `Select`. For keyset pagination, expose that column from a subquery so the cursor predicate in `WHERE` can reference it.

With a fixed keyset order, set the same `Direction` on the primary sort and each tie-breaker. Both `ORDER BY` and the `After` comparison use that direction even if the request asks for the opposite order. Leave every term at `SortDefault` to let the caller choose.

## Audit handwritten SQL and login roles before narrowing grants

Run these commands in the application repository using the application’s inventory, workload, scope and expected grant files. Set `TABULA_DSN` to a disposable PostgreSQL installation.

```sh
tabula check-login-role --role scope_login --member-of scope_reader,scope_writer --schemas public
tabula inventory --config inventory.json --bindings bindings.json --snapshot statements.sql > workload.json
tabula verify-roles --config workload.json
tabula grants --config privileges.json
tabula minimize --config trials.json
tabula check-coverage --config workload.json
tabula test-rls --config behavior.json
```

Generate `statements.sql` from the tables and views used by the assigned roles. A missing descriptor or aggregate makes `inventory` exit with the affected site ID. Review each unregistered dynamic site and each extra grant before changing the application's grant declarations. [Full JSON examples and interpretation](AUTHORIZATION.md)

## Physical sources with column grants

This declaration permits a plan to update `rank` when it finds a matching key. A role with schema usage, `SELECT(id)` and `UPDATE(rank)` can execute it.

```go
type Item struct {
	ID   int64 `db:"id,key"`
	Rank int32 `db:"rank"`
}
items := tabula.DefinePlanSource(reg, tabula.TableSpec[Item, int64]{
	Name: "public.items", RelationScope: tabula.Global(),
	WriteMode: tabula.WriteUpdateOnly, UpdateColumns: []string{"rank"},
})
i := tabula.FromTable(items, "i", tabula.Global())
id, rank := tabula.FieldOf(i, "id", tabula.Int8()), tabula.FieldOf(i, "rank", tabula.Int4())
key, value := tabula.Input("id", tabula.Int8()), tabula.Input("value", tabula.Int4())
change := tabula.DefineUpdateMany(tabula.RowsFrom(items, i), "change",
	tabula.MutationSpec{RowFilter: tabula.RowFilter{Where: tabula.Eq(id.Expr(), key.Expr())}},
	tabula.RowSet{Name: "rank", Assignments: []tabula.RowAssignment{tabula.Assign(rank, value.Expr())}})
n, err := change.Bind(session).UpdateMany(ctx, "rank", nil, tabula.BindValue(key, 1), tabula.BindValue(value, 5))
```

## Registered single-table reads and writes

Declare a physical table and an output view at startup. This example uses
`public.items(id bigint PRIMARY KEY, rank integer NOT NULL)` and a view with
one Go field, `ID int64` tagged `db:"id"`.

```go
rel := tabula.FromTable(items, "i", tabula.Global())
src := tabula.RowsFrom(items, rel)
id := tabula.FieldOf(rel, "id", tabula.Int8())
rank := tabula.FieldOf(rel, "rank", tabula.Int4())
minimum := tabula.Input("minimum", tabula.Int4())
byRank := tabula.Ge(rank.Expr(), minimum.Expr())

find := tabula.DefineRows(out, "search", src, tabula.RowsSpec{
    RowFilter: tabula.RowFilter{Optional: []tabula.RowPredicate{
        {Name: "minimum", When: byRank},
    }},
    WithExists: true,
    Orders: []tabula.RowOrder{{Name: "by_id", Terms: []tabula.RowTerm{tabula.Asc(id)}}},
    Pages:  tabula.RowPages{Offset: true, Keyset: true, MaxLimit: 50, MaxOffset: 500},
}, tabula.Project(tabula.Output(out, "id", tabula.Int8()), id.Expr()))

// Inside a callback with an authorized session.
choice := tabula.RowChoice{
    Filters: []string{"minimum"}, Order: "by_id", Page: tabula.FirstPage, Limit: 10,
}
bound := find.Bind(session)
rows, total, err := bound.List(ctx, choice, tabula.BindValue(minimum, 3))
// For a boolean answer, select the predicates without a page or count.
present, err := bound.Exists(ctx, choice.Filters, tabula.BindValue(minimum, 3))
```

`Find` executes the selected data statement. `Count` counts the unpaged result
for the selected predicates. `List` validates the bindings for both statements,
then runs the count and the page. Use a repeatable-read transaction when both
must see the same database snapshot. For a subsequent keyset page, call
`find.Cursor("by_id", tabula.At(id, last.ID))` with the last result. Set
`choice.Page` to `AfterPage` and `choice.After` to that cursor. Each keyset
order projects its terms and ends in the complete table key or grouping key.
Mixed directions and nullable terms use the same NULL ordering for comparisons
and sorting. Use `NullAt(field)` for a NULL cursor component.

With `RowsSpec.WithExists`, each filter and scope choice has one registered
`SELECT EXISTS`. It reads only the live-row and predicate columns, stops at
the first match, and needs no bindings from the projection, sort or page.
`IsNotDistinct` and `IsDistinct` compare nullable values and always return a
boolean, including when both values are NULL.

Use `And`, `Or` and `Not` to combine PostgreSQL boolean expressions.
A predicate declared with a custom Go codec still requires the catalog type
`pg_catalog.bool`. Nullable predicates follow PostgreSQL three-valued logic.

Project `CountAll`, `CountOf`, `MinOf`, `MaxOf`, `SumOf` or `AvgOf` to read
aggregate results through the view. Supply the PostgreSQL result type to
`SumOf` and `AvgOf`, for example `SumOf(rank.Expr(), Int8())` for an integer field.
A non-count aggregate needs a nullable destination. Put each free projected
field in `RowsSpec.GroupBy` when projecting an aggregate. An ungrouped scalar
aggregate has one result row even on an empty input. Grouped counts count
groups before the page bounds.

```go
next := tabula.Input("new_rank", tabula.Int4())
change := tabula.DefineUpdateMany(src, "change_rank", tabula.MutationSpec{
    RowFilter: tabula.RowFilter{Where: byRank},
}, tabula.RowSet{
    Name: "rank", Assignments: []tabula.RowAssignment{tabula.Assign(rank, next.Expr())},
})
affected, err := change.Bind(session).UpdateMany(ctx, "rank", nil,
    tabula.BindValue(minimum, 3), tabula.BindValue(next, 7))
```

Every mutation requires a fixed typed predicate. Optional predicates narrow it.
For a versioned table, supply `MutationSpec.Version` with
`ExpectVersion(revision, expected)`. Updates compare and advance the version,
and deletes compare it. A stale version can therefore yield zero affected
rows. The assignments may include the descriptor's soft-delete marker.
Subsequent reads and mutations exclude those deleted rows. Physical
`DefineDeleteMany` requires `TableSpec.AllowDelete`.

The default budget is 256 statements per plan. It includes the declared
scopes and independent predicate choices, each order and allowed page form,
and one count per scope and predicate combination on ordinary reads. Enabling
`WithExists` adds one existence check per combination. Each update assignment set
contributes its own shape. `TryDefineRows`, `TryDefineUpdateMany` and
`TryDefineDeleteMany` return declaration errors before publishing the family.

### Mapped table reads

Use `DefineTableRead(src, "labels", SelectColumns(label.Column()), Select().OrderBy(Asc(label)))` to apply explicit SELECT clauses to the table's Go type. The source supplies FROM and the selected column decoders in projection order.

Map the physical descriptor once, then name an occurrence for each operand. A field resolves against the same table registration each time. Declare a shared catalog default for a named Go type or override a single column when constructing the layout.

```go
type GroupID string
type Item struct {
    ID    int64   `db:"id,key"`
    Group GroupID `db:"group_id"`
    Note  *string `db:"note"`
}

reg := q.NewRegistry(q.RegistryConfig[struct{}]{
    Deny: q.DenyLegacySQL,
    DefaultTypes: []q.TypeDefault{q.TypeMapping(pg.UUID[GroupID]())},
})
items := q.DefinePlanSource(reg, q.TableSpec[Item, int64]{Name: "public.items", RelationScope: q.ByRLS()})
layout := q.MapTable(items)
src := layout.As("i")
group := q.MappedColumn[GroupID](layout, "group_id")
key := group.Input("group")
filter := q.RowFilter{Where: q.Eq(group.On(src.Rel()).Expr(), key.Expr())}

plan := q.DefineTableRows(src, "items_by_group", q.RowsSpec{RowFilter: filter}, q.SelectAll())
exists := q.DefineExists(src, "has_group", filter)
value := q.BindValue(key, groupID)
rows, err := plan.Bind(session).Find(ctx, q.RowChoice{}, value)
if err != nil {
    return err
}
found, err := exists.Bind(session).Exists(ctx, nil, value)
```

Here `q` denotes the root package and `pg` denotes `github.com/0xJean/tabula/pgtype`. Supply the request's `groupID` and handle each call's error before continuing. `SelectAll()` expands the table's declared fields. For a narrow read, pass `SelectColumns(field.On(src.Rel()).Column(), ...)`. The result has type `Item`, with unselected fields left at their Go zero values. A field requested by a predicate or an order still contributes its physical read grant. A self-join uses a second `layout.As("peer")` and the same field declarations on that occurrence.

| Read method | Empty result | Multiple matches |
| --- | --- | --- |
| `One` | `ErrNotFound`, which matches `sql.ErrNoRows` | `ErrMultipleRows` |
| `MaybeOne` | Zero value, false, nil | `ErrMultipleRows` |
| `First` | `ErrNotFound` | First row under the declared ordering policy |
| `MaybeFirst` | Zero value, false, nil | Same first row, true, nil |

Use `ReadForms{Exact: true, First: OrderedFirst}` with a declared order to bound the read on the server. Exact calls take up to two rows to detect duplicates. First calls take one. With both enabled, the methods share one registered shape per filter, order and scope. Supply the order name in `RowChoice` and omit pagination fields. To take any row of an unordered read, explicitly declare `UnorderedFirst`. Joined reads opt in with `ReadSelect.WithForms` and their bound methods use the statement's fixed order.

An unlocked, unpaged read can also call `One` or `MaybeOne` on its existing statement. It stops visiting rows at the second match. If PostgreSQL reports an error while the cursor closes, both drivers return that error with its SQLSTATE.

### PostgreSQL types and binding values

A layout first applies an explicit `ColumnType`, then checks for an exact registered Go type and finally uses a conventional built-in mapping. A nullable pointer may use its element's mapping. Declare named scalars, JSON, SQL arrays, enums and domains explicitly when the physical type is ambiguous. A table override can select `pg.Varchar(n)` to verify a length, or `pg.Varchar()` to check only the type OID. Select integer widths explicitly and use `pg.Numeric[string]()` to bind decimal text with its original precision.

Use `pg.JSONB[V]()` or `pg.JSON[V]()` for JSON input. A Go string encodes as a JSON string. To supply serialized JSON, use `json.RawMessage`. A nil raw message binds SQL NULL and a raw message containing `null` binds JSON null. `pg.Bytea()` handles binary data. `pg.ArrayOf(element)` declares a PostgreSQL array and selects the configured driver array adapter.

`BindValue` applies the slot's encoder once. Reuse the resulting binding for the data page, count, existence check or another session using the same slot. Built-in arrays and bytes preserve their input buffers. A custom `WithEncoder` function owns the stability of the value it returns. Keep its result independent of later changes to the caller's objects.

A column mapping selects its PostgreSQL type and input encoder. Results use the table field's scan target. For a `time` or `timetz` result, a field scanner can accept `time.Time` from lib/pq and clock text from pgx, including PostgreSQL's `24:00` boundary. An interval field can use `sql.NullString` to receive interval text with either driver.

## Explicit joins

Suppose `groups` and `jobs` are scoped table descriptors with bigint keys, and `out` is a view with `GroupID int64` and `JobID *int64`. Each call to `FromTable` creates a distinct occurrence. A self-join uses two occurrences of the same descriptor. Here the scope uses the named Go type `ScopeID` and the PostgreSQL type `bigint`.

```go
g := q.FromTable(groups, "g", q.ByScopeColumn())
j := q.FromTable(jobs, "j", q.ByScopeColumn())
groupID := q.FieldOf(g, "id", q.Int8())
jobID := q.FieldOf(j, "id", q.Int8())
jobGroup := q.FieldOf(j, "group_id", q.Int8())
keys := q.Input("keys", q.ArrayOf(q.Int8()))
scope := q.CatalogType[ScopeID]("pg_catalog", "int8", nil)
left := q.RowsFrom(groups, g, q.FieldOf(g, "scope_id", scope).Column())
right := q.RowsFrom(jobs, j, q.FieldOf(j, "scope_id", scope).Column())

plan := q.DefineRead(out, "group_jobs", q.Select(
    q.Project(q.Output(out, "group_id", q.Int8()), groupID.Expr()),
    q.Project(q.Output(out, "job_id", q.Int8()), jobID.Expr()),
).From(left).LeftJoin(right, q.Eq(groupID.Expr(), jobGroup.Expr())).
    Where(q.Any(groupID.Expr(), keys.Expr())).
    OrderBy(q.Asc(groupID), q.Asc(jobID)))

rows, err := plan.Bind(session).Find(ctx, q.BindValue(keys, []int64{1, 2}))
```

The right occurrence's scope and soft-delete conditions belong in `ON`. A group with only hidden jobs still produces one row with a nil job ID. A `Where` condition on the right occurrence filters the joined result after matching. To count visible jobs, project `CountOf(jobID.Expr())` and group by `groupID.Expr()`. Configure `RegistryConfig.ArrayArg` with `pqdriver.Array` for lib/pq, or use native slice encoding with pgx. An empty array matches zero rows.

Column-scoped occurrences in a join share one scope parameter. Use the same PostgreSQL scope type throughout the query. If it has a custom encoder, reuse the same type declaration across occurrences or set `RegistryConfig.ScopeID` for the registry.

## Registered locks and functions

These examples use `q` for the root package, a global `public.items` table and a one-column view named `entries` with an `int64` field tagged `db:"id"`. Configure the session adapter with the registry's `SQLAuthority()`. Execute locking reads inside a writable transaction.

```go
i := q.FromTable(items, "i", q.Global())
id := q.FieldOf(i, "id", q.Int8())
key := q.Input("key", q.Int8())
locked := q.DefineRows(entries, "lock_item", q.RowsFrom(items, i), q.RowsSpec{
    RowFilter: q.RowFilter{Where: q.Ge(id.Expr(), key.Expr())},
    Orders: []q.RowOrder{{Name: "by_id", Terms: []q.RowTerm{q.Asc(id)}}},
    Pages: q.RowPages{Keyset: true, MaxLimit: 1},
    Lock: q.ForRows(q.ForNoKeyUpdate, i, q.SkipLocked),
}, q.Project(q.Output(entries, "id", q.Int8()), id.Expr()))
choice := q.RowChoice{Order: "by_id", Page: q.FirstPage, Limit: 1}
rows, err := locked.Bind(session).Find(ctx, choice, q.BindValue(key, int64(7)))
```

This reads the first visible, available row at or above the bound key. `SkipLocked` moves past conflicting rows in the declared order. `NoWait` returns SQLSTATE `55P03` on conflict. `Wait` waits for acquisition. For a single locking read, declare `OrderedFirst` to lock at most one row or `Exact` to check up to two matches. Grouped or aggregate projections and `WithExists` fail at declaration. `Count`, `List` and `Exists` return `ErrOperationDenied` before executing a statement.

Column-scoped, RLS-enforced and soft-deleting tables use the same `RowsFrom` declaration for locking and ordinary reads. Pass `FieldOf(...).Column()` for a nonstandard scope or status type. Configured status values bind internally and are redacted. Boolean and timestamp markers use `= FALSE` and `IS NULL`.

For a transaction advisory lock, declare a one-boolean view. Each key binds separately. The two-integer key space is independent of the bigint key space.

```go
first, second := q.Input("first", q.Int4()), q.Input("second", q.Int4())
pair := q.AdvisoryXactLockPair(q.Shared, q.Try, first.Expr(), second.Expr())
lock := q.DefineAdvisoryLock(flags, "group_lock", pair)
held, err := lock.Bind(session).Acquire(ctx, q.BindValue(first, int32(1)), q.BindValue(second, int32(7)))
```

`Try` reports contention as `false, nil`. `Wait` reports successful acquisition as `true, nil`. Both commit and rollback release the lock. An explicit read-only transaction can acquire a transaction advisory lock. Direct accepts only reads without lock effects.

To call a scalar function, declare its signature and assign a typed expression to each formal. Reusing an input slot reuses its bind position. The function's declaration order determines the SQL argument order.

```go
find := q.DeclareRoutine(q.RoutineSpec[int64]{
    Schema: "public", Name: "lookup", Result: q.Int8(),
    Cardinality: q.RoutineOne, ReadOnly: true,
}, q.Parameter("id", q.Int8()))
lookup := q.DefineRoutine(entries, "lookup_item", find,
    q.Arg(q.FormalOf(find, "id", q.Int8()), key.Expr()))
row, err := lookup.Bind(session).One(ctx, q.BindValue(key, int64(7)))
```

Use `RoutineSet` and `All` for a set of values. Declare `Nullable` and a nullable view column for results that can be SQL NULL. The default function effect requires write authorization. A write call returning one or many results runs under the session's `Returning` policy. A command allowlist for writes rejects its outer `SELECT`. Verify each overload under its executing role. [Role readiness](AUTHORIZATION.md#check-a-role-at-startup)
