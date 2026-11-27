# M3 worked comparisons

These are proposed API sketches, not compiled examples or shipped functions. In each pair, SQL shows the intended PostgreSQL operation; the typed version shows declarations using the approved vocabulary. Existing setup is shared: `i`, `n`, `e` are RowSource values from TableLayout.As; their fields are existing MappedColumn handles; output fields belong to a final View; inputs are existing Input handles. All ordinary values shown as `$n` use BindValue at execution. No intermediate Go row type is required.

## A correlated boolean and a nullable scalar

```sql
SELECT i.id,
       EXISTS (SELECT 1 FROM notes n WHERE n.item_id = i.id) AS has_notes,
       (SELECT n.label FROM notes n WHERE n.item_id = i.id
        ORDER BY n.id DESC LIMIT 1) AS latest
FROM items i;
```

```go
hasNotes := tabula.ExistsQuery(tabula.Select().From(n).
    Where(tabula.Eq(noteItemID.Expr(), tabula.Outer(itemID.Expr()))))
latest := tabula.ScalarSubquery(tabula.SelectValue(noteLabel.Expr()).From(n).
    Where(tabula.Eq(noteItemID.Expr(), tabula.Outer(itemID.Expr()))).
    OrderBy(tabula.Desc(noteID)).Take(1))
query := tabula.Select(
    tabula.Project(outID, itemID.Expr()),
    tabula.Project(outHasNotes, hasNotes),
    tabula.Project(outLatest, latest),
).From(i)
plan := tabula.DefineRead(result, "item_notes", query)
rows, err := plan.Bind(session).Find(ctx)
```

The final Go field for latest is nullable. Removing Take keeps PostgreSQL's many-row error, rather than choosing a row. `Outer` is checked against the enclosing occurrence, not the string alias. The boolean remains an ordinary Pred/Expr[bool] usable in either Project or Where.

## Rank within a group, then filter the result

```sql
SELECT r.id, r.position
FROM (
  SELECT i.id, row_number() OVER (PARTITION BY i.group_id ORDER BY i.rank, i.id) AS position
  FROM items i
) r
WHERE r.position <= $1
ORDER BY r.id;
```

```go
idResult := tabula.Export("id", itemID.Expr())
position := tabula.Export("position", tabula.Over(tabula.RowNumber(), tabula.WindowSpec{
    Partition: []tabula.Scalar{itemGroup.Expr()},
    Order: []tabula.RowTerm{tabula.Asc(itemRank), tabula.Asc(itemID)},
}))
ranked := tabula.Derived("r", tabula.Select(idResult.Project(), position.Project()).From(i))
query := tabula.Select(
    tabula.Project(outID, idResult.From(ranked)),
    tabula.Project(outPosition, position.From(ranked)),
).From(ranked).Where(tabula.Le(position.From(ranked), take.Expr())).
    OrderBy(tabula.AscExpr(idResult.From(ranked)))
plan := tabula.DefineRead(result, "group_positions", query)
rows, err := plan.Bind(session).Find(ctx, tabula.BindValue(take, int64(2)))
```

The exported handles carry SQL types and codecs into the derived source. There is no duplicated rank struct or virtual physical table. An outer list family may paginate the filtered result; the seek must not move below the window calculation.

## Insert or update, with one registered returning statement

```sql
INSERT INTO items AS i (id, label) VALUES ($1, $2)
ON CONFLICT (id) DO UPDATE SET label = excluded.label
RETURNING i.id, i.label;
```

```go
write := tabula.InsertInto(i).Values(
    tabula.Assign(itemID, idInput.Expr()),
    tabula.Assign(itemLabel, labelInput.Expr()),
).OnConflict(tabula.ConflictColumns(itemID.Column())).
    DoUpdate(tabula.Assign(itemLabel, tabula.Excluded(itemLabel)))
returning := tabula.WithReturning(write,
    tabula.Project(outID, itemID.Expr()), tabula.Project(outLabel, itemLabel.Expr()))
plan := tabula.DefineReturning(result, "put_item", returning)
rows, err := plan.Bind(session).All(ctx,
    tabula.BindValue(idInput, int64(7)), tabula.BindValue(labelInput, "note"))
```

The plan requires the actual INSERT, UPDATE and SELECT columns, including the conflict target and excluded value. An append-only target uses targetless DoNothing without RETURNING; it does not gain SELECT through this example. A failed operation cannot leave a partially decoded result and commit its writes silently.

## A conditional update feeding an insertion

```sql
WITH changed AS (
  UPDATE items AS i SET rank = i.rank + $1 WHERE i.id = $2
  RETURNING i.id, i.rank
)
INSERT INTO entries (id, rank) SELECT id, rank FROM changed;
```

```go
changedID := tabula.Export("id", itemID.Expr())
changedRank := tabula.Export("rank", itemRank.Expr())
update := tabula.UpdateOf(i).
    Set(tabula.Assign(itemRank, tabula.Plus(itemRank.Expr(), delta.Expr()))).
    Where(tabula.Eq(itemID.Expr(), wantedID.Expr()))
changed := tabula.WriteCTE("changed", tabula.WithReturning(update,
    changedID.Project(), changedRank.Project()))
c := changed.As("c")
copyID := tabula.Export("id", changedID.From(c))
copyRank := tabula.Export("rank", changedRank.From(c))
insert := tabula.InsertInto(e).Columns(entryID.Column(), entryRank.Column()).
    FromSelect(tabula.Select(copyID.Project(), copyRank.Project()).From(c)).With(changed)
plan := tabula.DefineWrite(e, "record_changes", insert)
count, err := plan.Bind(session).Exec(ctx,
    tabula.BindValue(delta, int32(1)), tabula.BindValue(wantedID, int64(7)))
```

WITH is attached to the outer INSERT, not to an embedded subquery. The returned count is the INSERT count. Both physical write commands are registered and role checked. The INSERT is dependent on the returned stream; there is no claimed ordering between independent sibling modifications.

## Multiple lock strengths in a single SELECT

```sql
SELECT i.id, n.id
FROM items i JOIN notes n ON n.item_id = i.id
ORDER BY i.id, n.id LIMIT $1
FOR UPDATE OF i NOWAIT FOR SHARE OF n SKIP LOCKED;
```

```go
query := tabula.Select(tabula.Project(outID, itemID.Expr()), tabula.Project(outNoteID, noteID.Expr())).
    From(i).InnerJoin(n, tabula.Eq(noteItemID.Expr(), itemID.Expr())).
    OrderBy(tabula.Asc(itemID), tabula.Asc(noteID)).Limit(take, 32).
    Locks(tabula.ForRows(tabula.ForUpdate, i.Rel(), tabula.NoWait),
        tabula.ForRows(tabula.ForShare, n.Rel(), tabula.SkipLocked))
plan := tabula.DefineRead(result, "locked_items", query)
rows, err := plan.Bind(session).Find(ctx, tabula.BindValue(take, int64(1)))
```

Each physical target contributes its own UPDATE privilege and policy need. Two separate session calls are never substituted. The count/list pairing remains unavailable for this locking read.

## Materialize an array before comparing it twice

```sql
WITH generated AS MATERIALIZED (
  SELECT ARRAY[pg_catalog.gen_random_uuid()] AS value FROM items
)
SELECT CASE WHEN value OPERATOR(pg_catalog.=) $1::uuid[]
            THEN NULL::uuid[] ELSE value END AS result
FROM generated;
```

```go
value := tabula.Export("value", tabula.ArrayExpr(tabula.GenRandomUUID()))
generated := tabula.CTE("generated", tabula.Select(value.Project()).From(i), tabula.Materialized)
g := generated.As("g")
query := tabula.Select(tabula.Project(outArray,
    tabula.NullIf(value.From(g), otherArray.Expr()))).From(g).With(generated)
plan := tabula.DefineRead(result, "generated_values", query)
rows, err := plan.Bind(session).Find(ctx, tabula.BindValue(otherArray, []string{}))
```

The CTE is explicitly part of the declared query. No key column is added just to force correlation. Repeated references to value read the same materialized slot, while its volatile provenance still disqualifies it as a stable page key. A truly correlated lateral alternative can use an already declared outer key; an uncorrelated lateral must not be described as per-row execution.

## A schema-only partial conflict predicate

```sql
INSERT INTO items(id, rank, active) VALUES ($1, $2, $3)
ON CONFLICT (rank) WHERE active AND id > 1 DO NOTHING;
```

```go
target := tabula.ConflictColumns(itemRank.Column()).WhereIndex(
    tabula.IndexAnd(tabula.IndexTrue(itemActive), tabula.IndexGt(itemID, int64(1))))
write := tabula.InsertInto(i).Values(
    tabula.Assign(itemID, idInput.Expr()), tabula.Assign(itemRank, rankInput.Expr()),
    tabula.Assign(itemActive, activeInput.Expr()),
).OnConflict(target).DoNothing()
plan := tabula.DefineWrite(i, "append_active", write)
```

The fixed 1 belongs to an existing schema index predicate and cannot be replaced by a request Slot. The three row values remain bound. This is the approved schema-value boundary for index inference, not a general SQL literal facility. The reference probe checks why a forced generic plan cannot rely on the runtime value of a partial-index predicate parameter.

## API consistency checks

| Concern | Proposed rule |
| --- | --- |
| Registration | Existing Define/TryDefine convention; no execution from a builder. |
| Session binding | All executable plans use Bind(session); no new direct session argument. |
| Intermediate schemas | Exported handles or existing OutputField identities; no fake View. |
| Mapping | TableLayout supplies physical handles; table selections reuse existing decoders. |
| Predicates | Pred remains Expr[bool]; no conversion method or string predicate overload. |
| Slots | One canonical value snapshot; each statement adapts its own dense layout. |
| Generics | New result type parameters appear on free functions, not generic methods. |
| Iteration | Read iterators stay reads; returning iterators require write authorization. |
| Cardinality | Read One/First contracts stay intact; writes are not silently narrowed. |
| SQL text | No public Raw, operator/function-name injection, or general literal expression. |
| Scope | Physical leaves own scope/RLS; aliases and intermediate relations do not create scopes. |
