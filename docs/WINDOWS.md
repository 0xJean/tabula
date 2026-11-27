# Windows, row locks and network addresses

Window functions retain the rows of their input query. Wrap a ranking/value
function or an ordinary aggregate in `Over`, then project it or use it in the
final `OrderBy`. `WindowSpec.Partition` lists typed expressions and `Order` uses
the existing `RowTerm` direction and NULL placement. No implicit order or frame
is added.

```go
position := tabula.Over(tabula.RowNumber(), tabula.WindowSpec{
    Partition: []tabula.Scalar{groupID.Expr()},
    Order: []tabula.RowTerm{tabula.Asc(rank), tabula.Asc(id)},
})
```

`Rank`, `DenseRank`, `PercentRank`, `CumeDist` and `NTile` preserve PostgreSQL's
peer and bucket behavior. `Lag` and `Lead` require an explicit int4 offset and
a default expression of the value's exact type. Pass `TypedNull` for a NULL
default. Negative lag/lead offsets retain PostgreSQL's reverse-direction
behavior. `FirstValue`, `LastValue` and `NthValue` inspect the frame; missing
rows and empty frames produce SQL NULL. PostgreSQL respects NULLs in these
functions. There is no unsupported IGNORE NULLS option.

An ordinary aggregate can be used as a window aggregate, including a legal
`FilterWhere`. `DistinctInputs` and `AggregateOrder` are rejected inside `Over`
because PostgreSQL 14 does not implement those window aggregate forms. Ordinary
aggregation can feed the subsequent window phase. A window cannot feed another
same-level window or an ordinary aggregate, and cannot appear in WHERE, HAVING
or GROUP BY. To filter window results, export them from a derived query and
filter that result.

`WindowSpec.Name` optionally shares a full specification between several
functions in one SELECT. Repeated names must have exactly the same partition,
ordering, frame and expression identities. The compiler emits one WINDOW
declaration, and independent query blocks may use the same name.

## Explicit frames

`WholePartition()` declares ROWS from the first through the last partition row.
`RunningRows()` declares ROWS from the first through the current row. Leaving
Frame zero retains PostgreSQL's default RANGE frame, which ends at the last
peer of the current row when ordered. In particular, default `LastValue` need
not return the partition's final value.

```go
frame := tabula.WindowFrame{
    Mode: tabula.FrameGroups,
    Start: tabula.Preceding(distance.Expr()), // exact int8 input
    End: tabula.CurrentRow(),
    Exclude: tabula.ExcludeTies,
}
```

`FrameRows`, `FrameRange` and `FrameGroups` support unbounded, current,
preceding and following bounds. `ExcludeNoOthers`, `ExcludeCurrentRow`,
`ExcludeGroup` and `ExcludeTies` retain their native semantics. GROUPS requires
ORDER BY. RANGE with an offset requires exactly one ordering expression and
a reviewed native `in_range` overload. Small integers accept the supported
integer offset overloads; int8 requires int8. Numeric requires numeric, float4
and float8 require float8, and date/time/timestamp/interval accept interval.
Use explicit `Cast` when a bound has a different SQL type.

ROWS and GROUPS offsets require exact int8. Bounds cannot contain row fields,
subqueries, aggregates or windows. Dynamic negative/NULL offsets and invalid
runtime bucket/position values preserve PostgreSQL's errors. No bound is
interpolated into SQL. Every selected native function signature contributes
its exact EXECUTE readiness requirement.

## Result pagination

Read-list pages apply seek after window computation, so a cursor does not
change partitions or reassign positions. Counts retain the semantic unpaged
cardinality, including explicit `Take` or `Limit` bounds inside the query.
The page's limit and cursor apply outside that result. Private fixed limits
need no caller binding, and data/count layouts each bind the original inputs.
A window number is never itself evidence of a unique result key.
Window-derived cursor values require a deterministic partition order, genuine
input/result-key premises, deterministic collations and reproducible value
representations. Bounded offset pages remain available when no keyset proof
can be established. Stable clocks and volatile values do not become traversal
keys merely because a query materializes them.

Derived queries and common tables preserve genuine exported keys and their
catalog premises. GROUP BY and duplicate-eliminating sets can establish a
result key from the complete equality tuple. UNION ALL additionally needs
disjoint declaration-time discriminator tuples and each arm's complete key;
runtime input values cannot establish disjoint arms. Plain UNNEST and recursive
sources have no automatic result key. Grouping or duplicate elimination can
establish one after an array expansion without assuming its values are unique.
An explicit limit preserves traversal proof only when a total result order
determines its subset, or the input contains at most one row. An unordered
limited query remains available for ordinary reads and bounded offset pages.

## Multiple physical locks

```go
query = query.Locks(
    tabula.ForRows(tabula.ForUpdate, items.Rel(), tabula.NoWait),
    tabula.ForRows(tabula.ForShare, notes.Rel(), tabula.SkipLocked),
)
```

The clauses appear in declaration order, each with explicit OF and its own
wait mode. Each target requires SELECT and UPDATE permission on at least one
column, plus the corresponding RLS policies. Targets must be distinct visible
physical occurrences in that block. A nullable join side or aggregate,
distinct, set or window result cannot be locked. Lock an eligible inner
physical query explicitly before deriving its result. `Lock` replaces the
entire prior lock list, and `Lock(RowLock{})` removes it. Exact counts and
paired List calls are unavailable for locking reads.

The existing transaction advisory API supports Shared and Exclusive with
bigint or two-int4 keys. Shared/shared holders coexist; a conflicting exclusive
holder prevents acquisition. Wait and Try retain their existing contracts.
Both modes release on transaction commit or rollback, are allowed in a
read-only transaction and are refused by Direct. No multi-key acquisition
ordering is implied by a SELECT projection.

## Host addresses

`Host(value)` accepts the exact native inet identity, using either
`pgtype.Inet()` or `pgtype.InetPrefix()`. It returns text containing the address
without the prefix, preserves SQL NULL, and records
`pg_catalog.host(inet)` EXECUTE. A cidr, domain or text operand is rejected
unless explicitly converted to the required native identity. It does not
introduce a second network mapping or accept a function-name string.

## Native scalar composition

`HashTextExtended(value, seed)` calls `pg_catalog.hashtextextended(text,int8)`.
It accepts the reviewed text family and an exact int8 seed, preserves NULL,
and records the native function's EXECUTE requirement. Compose the returned
int8 with the existing advisory lock API when the server's native hash is the
desired key. No Go hashing algorithm or cross-version hash promise is implied.

`PlusAs(pgtype.Timestamp(), date, time)` and the symmetric time/date form
produce the native timestamp without time zone. Both operands retain NULL
propagation. A different declared result type is rejected during definition.
