# Typed writes and returned rows

Typed write plans live in `github.com/0xJean/tabula/plan` (`package plan`). Declare writes from the same mapped physical occurrences used by typed reads.
`InsertInto`, `UpdateOf` and `DeleteFrom` build immutable declarations; they do
not execute or register SQL. `DefineWrite` registers a count-only statement and
requires its owner to be the outer target. `DefineReturning` registers an exact
ordered result projection into a `tabula.View`. Every `Define` constructor has a `Try`
partner that publishes no shapes on failure.

```go
write := plan.UpdateOf(items).
    Set(plan.Assign(rank, newRank.Expr())).
    Where(plan.Eq(id.Expr(), wantedID.Expr()))
writePlan := plan.DefineWrite(items, "set_rank", write)
count, err := writePlan.Bind(session).Exec(ctx,
    plan.BindValue(newRank, int32(4)), plan.BindValue(wantedID, int64(7)))
```

Assignments are evaluated on the old row. Assignment targets contribute UPDATE
privileges, physical types and secrecy, but do not acquire SELECT merely because
they are overwritten. Actual predicates, expressions, matching sources and
returned fields contribute their own reads. Keys, once, scope and version fields
remain protected. Versioned mutations require `.Version(ExpectVersion(...))`
and increment the version automatically. Physical deletion requires AllowDelete;
the descriptor's live-row filter continues to exclude deleted rows. Use
`.AllRows()` to explicitly affect every authorized live row.

## Insertion and defaults

Each `.Values(assignments...)` adds one fixed row with the same ordered targets.
`.DefaultValues()` uses native defaults. `.Columns(...).FromSelect(query)` maps
the read query's ordered exports to exact target types and nullability. Serial,
identity and generated values may be omitted or supplied with `DefaultOf(field)`
where PostgreSQL permits it. `DefaultOf` is an assignment, not a general SQL
expression. Default metadata is verified from catalogs; runtime tests still
establish trigger, function and sequence permission behavior.

Column-scoped insertion automatically supplies the session scope once per row.
It rejects caller assignments to that column. A separately declared
`.UnscopedOnly()` insertion requires an explicit scope value and an unscoped
session. ByRLS never adds a scope predicate or session parameter.

## Conflicts and partial indexes

`.DoNothing()` without a target preserves INSERT-only privileges. Select an
arbiter with `.OnConflict(ConflictColumns(...))` or
`.OnConflict(ConflictConstraint("items_pkey"))`, then choose DoNothing or
DoUpdate. `.UpdateWhere(predicate)` restricts the conflict update. `Excluded`
is valid only in the update arm of that exact target. Protected-column rules,
version checks, scope and liveness filters also apply to that arm.

Partial-index inference uses `WhereIndex` and the separate `IndexPredicate`
grammar. IndexEq/Ne/Lt/Le/Gt/Ge accept fixed schema facts with reviewed native
integer, boolean or text types. IndexTrue/False/Null/NotNull and IndexAnd/Or/Not
compose schema predicates. Secret columns, custom encoders, request slots and
arbitrary SQL are excluded. The renderer escapes schema text independently of
session string settings. Ordinary request values always remain bound.

Verification uses the original statement's EXPLAIN conflict arbiters and their
catalog properties, including partial-index inference under generic planning.
DoUpdate requires an immediate unique arbiter. DoNothing can name an eligible
exclusion constraint. A declaration is not itself catalog evidence.

## Matching sources and modifying common tables

UPDATE `.From(source, on)` and DELETE `.Using(source, on)` use explicit sources.
Value-bearing matches require equality covering an immediate non-null physical
unique key or an actual derived result key. Physical uniqueness is an explicit
catalog verification requirement. UNNEST and ordinality do not assert uniqueness;
group or deduplicate an exported Derived result when needed. Target-only DELETE
results do not depend on which matching source row is chosen.

`WriteCTE(name, WithReturning(write, exports...))` exposes changed rows.
`EffectCTE(name, write)` declares a result-less effect that executes even when
unused. Attach either with the outer statement's `.With(...)`. A result-less
common table cannot be a source. Independent modifications of one physical
table are rejected. Each WriteCTE/EffectCTE call declares an independent effect;
references to the same CommonTable reuse its one execution. Batch and
UnscopedOnly execution families belong to the outer statement. Use returned
rows for dependencies: sibling modifications
are not a sequential program, and base-table reads use the statement snapshot.
Counts and rows belong to the outer command, never an invented sum of effects.

The full physical command/effect graph participates in authorization, including
unused modifying CTEs. A SELECT containing writes uses returning-write authority;
it cannot be registered or executed as an ordinary read. Read-only sessions and
Direct refuse it. Active method policies need their explicit signed-write CTE
opt-in and all underlying write commands permitted.

## Results, streams and batches

`WithReturning(write, projections...)` returns new UPDATE rows and old DELETE
rows, including trigger changes. `DefineTableReturning(source, name, write,
selection)` reuses the mapped table decoder. SelectColumns can reorder or omit
fields; omitted Go fields remain zero. Finite mutation families use
DefineUpdateReturning/DefineDeleteReturning, with UpdateMany/DeleteMany and
UpdateIter/DeleteIter on their bound handles.

All returns every row or no partial result on failure. Iter does not apply a
read-style limit. Early break stops decoding and yielding, then drains the server
statement to its final outcome. A late failure rolls the operation back and
fails the enclosing transaction with `ErrUnobservedWrite`, preserving the database
diagnostic; an iterator that has already stopped cannot yield another error.
Stop pull iterators before leaving the runner. A suspended iterator is closed by session cleanup and
must not permit an unfinished write to commit.

`.Batch(1, 16, 128)` declares a finite set of VALUES row counts from one template.
Include 1 for arbitrary remainders. ExecBatch or AllBatch takes a slice of binding
rows and optional common bindings used outside the VALUES template. Empty input
does no I/O. Every original row is validated before the first database operation;
each chunk independently adapts canonical bindings and stays within PostgreSQL's
65,535-parameter limit. All chunks roll back together on failure. A batch template
does not include modifying common tables or another executable family.

RETURNING and batch execution require `OperationSession`. Both shipped drivers
provide a sealed savepoint child; an external driver without that capability
receives ErrOperationRequired before I/O. No raw SAVEPOINT fallback is used.
Driver, cancellation and decoding failures roll back the operation. Returned rows
have no promised positional association with batch inputs: triggers and conflicts
can remove rows and PostgreSQL does not promise RETURNING order. Project an input
key when association matters.

Write results require non-executing protocol description during verification.
Never execute a DML statement, including a write CTE under LIMIT 0, to discover
its result types. Wire types and logical domain lineage are separate evidence.
