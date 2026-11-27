# Composing typed queries

Typed query composition lives in `github.com/0xJean/tabula/plan` (`package plan`). Queries retain their physical descriptors, PostgreSQL types, codecs and input
identities across nesting. An intermediate relation needs exported handles; only
the final executable read needs a `tabula.View` and its Go decoder. Constructing a query
performs no database access and registers no statement.

```go
value := plan.Export("value", itemID.Expr())
selected := plan.Derived("selected", plan.Select(value.Project()).From(items))
query := plan.Select(plan.Project(outValue, value.From(selected))).From(selected)
readPlan := plan.DefineRead(output, "selected_items", query)
```

The exported identity belongs to `value`, not its name. Another `Export("value",
...)` cannot read this field. `ResultOf` instead reuses an existing `OutputField`
handle. `QuerySource.As` makes a distinct occurrence of the same result schema.

## Lexical references and subqueries

Ordinary fields must belong to the current query block. `Outer(expression)`
looks exactly one level out; nest it for a grandparent. Aliases may repeat across
blocks without capturing another occurrence. A lateral join sees preceding
inputs only. An ordinary derived source cannot capture enclosing fields.

```go
latest := plan.ScalarSubquery(plan.SelectValue(noteLabel.Expr()).From(notes).
    Where(plan.Eq(noteItemID.Expr(), plan.Outer(itemID.Expr()))).
    OrderBy(plan.Desc(noteID)).Take(1))
```

A scalar query produces NULL on zero rows and PostgreSQL error `21000` on more
than one row. The final decoder must admit NULL. `Take` binds a private fixed
limit; `Limit(input, maximum)` validates a caller's nonnegative int8 limit before
I/O. Neither creates another executable family inside the query.

`ExistsQuery`, `InQuery`, `AnyQuery` and `AllQuery` preserve PostgreSQL's boolean
and NULL semantics. Comparisons use the closed `ComparisonOp` set and qualified
native operators. An empty ANY result is false; an empty ALL result is true.
`Not(InQuery(...))` retains NULL rather than treating it as false. Aggregates over
outer-only fields must be computed and exported in their owning block first.

A source-free `Select` can contain inputs, scalar functions and subqueries. Its
physical grant inventory is empty unless its nested queries read relations.
WHERE, aggregation and limits still determine its actual cardinality.

## Sets and common tables

`Union`, `UnionAll`, `Intersect`, `IntersectAll`, `Except` and `ExceptAll` preserve
explicit nesting and arm-local clauses. Arms must have the same arity, exact SQL
types, Go representations and codecs. The first arm supplies exported identities;
nullability widens across arms. Use `Derived` to filter or order the final result.

Attach common tables explicitly with `With`. Their order defines lexical
visibility; a reference without its declaration or to a later declaration fails
atomically. `Materialized` stores output values for repeated references within
one result row. It preserves volatile provenance and does not establish a stable
page key. `DefaultMaterialization` and `NotMaterialized` provide no such guarantee.

```go
value := plan.Export("value", plan.ArrayExpr(plan.GenRandomUUID()))
generated := plan.CTE("generated", plan.Select(value.Project()).From(items),
    plan.Materialized)
g := generated.As("g")
query := plan.Select(plan.Project(outArray,
    plan.NullIf(value.From(g), otherArray.Expr()))).From(g).With(generated)
```

Direct volatile-array `NullIf` remains rejected. The explicit stored field above
can be read repeatedly while referring to the same generated array in its row.
A lateral keyword alone does not guarantee per-row execution.

A common table nested inside a lateral query can explicitly depend on the
preceding outer row. Its body cannot capture the containing query's local FROM
sources; it retains the already-visible ancestor chain. Use `Outer(Outer(id))`
inside that body to reference the preceding source. A correlated `Materialized`
body then stores one result per outer row, so repeated references share that
row's generated value while different outer rows are evaluated separately.

`RecursiveCTE` accepts a seed, `RecursiveUnionDistinct` or `RecursiveUnionAll`, and
a step callback receiving a scoped self source. The self reference is legal only
once as a direct step input. Supply a termination predicate and a cancellable
execution context. Recursive fields are conservatively nullable. Mutual recursion,
SEARCH/CYCLE decorators and native record paths are outside the supported surface.

## Arrays and binding reuse

`UnnestColumn` declares an array and its exact scalar element mapping. `Unnest`
expands heterogeneous parallel arrays with NULL padding; every element output is
nullable. `UnnestOrdinality` supplies int8 position without promising a persistent
key. SQL uses qualified `pg_catalog.unnest` overloads inside `ROWS FROM`.

`ArraySubquery` constructs `ARRAY(SELECT ...)`, including an empty non-NULL array
for empty input. Nullable elements require `ArraySubqueryAs` with a mapping such
as `ArrayOfNullable`. Nested array elements are rejected rather than silently
changing dimensions.

Bind each distinct input identity once. Reusing one input across blocks shares
one parameter; independently declared inputs remain distinct even when their
names match. New graph snapshots include deterministic position labels to keep
these inputs unambiguous. Existing flat queries retain their prior SQL, labels
and rejection of different inputs with the same name. A canonical `Binding` can
be reused across query shapes; encoders run at binding construction and driver
array adaptation uses a separate snapshot on each execution.
