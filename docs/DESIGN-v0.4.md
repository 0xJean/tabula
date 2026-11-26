# Typed PostgreSQL queries for v0.4

**Design for the v0.4 series.** A caller should be able to express a whole data statement in Go, including an explicit join, and inspect the exact PostgreSQL statement that will run. A query owns its result shape, inputs, permitted alternatives and authorization metadata. The registry and session execute it. Common use cases include multitable reads, grouped reports, work selection with row locks, coordination with transaction advisory locks, and writes to append-only audit tables.

This proposes one change to the design principles. A caller may specify both sides, the kind and the predicate of a join as typed operands. Tabula may render that join. Relations in struct tags, automatic path selection, preloading, lazy loading and DDL generation remain outside the proposal. The implementation updates the join rule in the contributor guide, design principles, README and roadmap together. [Current design](DESIGN.md) · [Contributor constraints](../AGENTS.md)

## An explicit join from declaration to execution

The v0.4.0 foundation supports single-table operations. This example illustrates the same types extended by the subsequent explicit-join release. The proposed public types and constructors live in the root module. In the sketches, `q` aliases `github.com/0xJean/tabula`. `groups` and `jobs` are existing table descriptors with a scope column. `summaries` is an existing `View[Summary, S]` with two non-null `int64` result fields. The caller wants groups with at least a given number of jobs, including empty groups when the minimum is zero.

```go
g := q.FromTable(groups, "g", q.ByScopeColumn())
j := q.FromTable(jobs, "j", q.ByScopeColumn())
groupID := q.FieldOf(g, "id", q.Int8())
jobID := q.FieldOf(j, "id", q.Int8())
jobGroup := q.FieldOf(j, "group_id", q.Int8())
minimum := q.Input("minimum", q.Int8())
jobCount := q.CountOf(jobID.Expr())

query := q.DefineRead(summaries, "groups_by_size",
    q.Select(
        q.Project(q.Output(summaries, "group_id", q.Int8()), groupID.Expr()),
        q.Project(q.Output(summaries, "job_count", q.Int8()), jobCount),
    ).From(g).
        LeftJoin(j, q.Eq(groupID.Expr(), jobGroup.Expr())).
        GroupBy(groupID.Expr()).
        Having(q.Ge(jobCount, minimum.Expr())).
        OrderBy(q.Asc(groupID.Expr())),
)

// In a runner callback.
rows, err := query.Bind(session).Find(ctx, q.BindValue(minimum, int64(2)))
```

The scoped snapshot for this shape is shown below. `$1` comes from the verified session and `$2` from `minimum`. Counting the right-hand ID gives zero for an unmatched group. Moving the right-hand scope condition to the outer `WHERE` would lose that group.

```sql
SELECT "g"."id" AS "group_id", count("j"."id") AS "job_count"
FROM "app"."groups" AS "g"
LEFT JOIN "app"."jobs" AS "j"
  ON ("g"."id" = "j"."group_id") AND ("j"."scope_id" = $1)
WHERE ("g"."scope_id" = $1)
GROUP BY "g"."id"
HAVING (count("j"."id") >= $2)
ORDER BY "g"."id" ASC
```

When the database exclusively enforces row scope, declare the physical table with the RLS strategy. For example, `j := q.FromTable(jobs, "j", q.ByRLS())` can use an explicit request key as its only parameter. The registered SQL for that declaration is:

```sql
SELECT "j"."id" AS "job_id"
FROM "app"."jobs" AS "j"
WHERE ("j"."id" = $1)
```

Here `$1` binds the requested job ID. `ByRLS` emits no scope predicate and reserves no scope slot. The plan records `app.jobs` as requiring ENABLE and FORCE ROW LEVEL SECURITY. Startup checks the declared physical relation and the executing role, including bypass and ownership considerations. A real-database test under that role must read an allowed row and exclude an otherwise matching row in another scope. Missing policies or a missing FORCE flag must fail verification. Apply these checks independently to each occurrence in a join.

### Types and validation

| Proposed API | Contract |
| --- | --- |
| `FromTable[T,K,S](*Table[T,K,S], alias, scope) Rel[T]` | Each call creates a distinct relation occurrence. A self-join needs two occurrences. A column retains its owner, PostgreSQL type and nullability. |
| `FieldOf[R,V](Rel[R], name, PGType[V]) Field[V]` | The name must match a column of the descriptor. The declared Go type or registered codec must match `V`. A field offers `Expr()` for reading and can serve as a write target on its own table. Existing `Column[T]` continues to describe a Go row accessor. |
| `Input[V](name, PGType[V]) Slot[V]`, `BindValue[V](Slot[V], V)` | Reuse a slot to reuse one parameter. Bindings belong to one plan. Missing, duplicate, foreign and surplus bindings fail before I/O. An untyped nil has no inferred SQL type. |
| `Eq[V](Expr[V], Expr[V]) Pred`, `Cast[To](to, expr)`, `Project[V](OutputField[V], Expr[V])` | Go rejects differing value types for equality or projection. Definition also checks the SQL operator signature, cast, collation and nullable destination. `uuid` and `text` can share a Go representation and still fail this check. |
| `DefineRead(view, key, select)`, `DefineWrite(table, key, mutation)` | Startup constructors freeze and register the checked tree atomically. The corresponding error-returning constructors validate without leaving a partially registered definition. Mutations with results also declare their output view. |

`PGType[V]` pairs a Go codec with a PostgreSQL type identity. Built-in constructors cover integer widths, boolean, text, UUID codecs, byte strings, date and time, numeric, JSON/JSONB, network, interval and arrays. A custom domain, enum, range or extension type needs an explicit schema-qualified descriptor and codec. For an existing table whose Go type does not uniquely identify a PostgreSQL type, declare the mapping once, share it across occurrences and verify it against the migration-built catalog. Conflicting declarations on the same column fail at definition. A fixed, read-only relation declaration can describe a catalog or physical view without a primary key. A result-only `View` is a projection and becomes a FROM source only through a declared catalog relation or a typed subquery.

At definition time the compiler resolves the closed graph against descriptor metadata. It checks registry identity, alias visibility, duplicate output names, arity, type and permissible coercion. A correlated reference needs an explicit outer relation. A left join lifts the right side to nullable in the result, including a right field declared non-null on its base table. `COALESCE`, aggregates and outer joins propagate nullability into the existing view scanner. A nullable projection requires a nullable destination or a checked operation that removes nulls. SQL predicates use three-valued logic. `IsNull` and `IsNotDistinct` have explicit constructors.

Actual database types, overloaded functions, grants and result descriptions require a catalog. CI verifies these against the migration-built PostgreSQL schema. A misspelled descriptor field fails at startup. A real column altered behind a correct descriptor fails database verification. Hot execution uses the frozen binder and the existing decoder. The compiler, metadata and renderer use the standard library only. Heterogeneous nodes carry private type witnesses. Public structs expose no SQL-rendering hooks.

## PostgreSQL surface

| Construct | Proposed form and validation |
| --- | --- |
| Boolean, numeric, text and row expressions | `Eq/Ne/Lt/Le/Gt/Ge`, `And/Or/Not`, `Between`, `IsDistinct/IsNotDistinct`, `In`, typed arithmetic, `Like/ILike`, regex matching, concatenation, tuples, `Case/When/Else`, `Coalesce`, `NullIf` and `Greatest/Least`. Operators are closed signatures. CASE arms require one result type, with explicit casts for widening. |
| PostgreSQL-specific operators | Typed JSON value/text/path extraction, key existence, containment and builders. Typed array `Any/All`, containment, overlaps, construction and subscripts. Separate interval arithmetic, range construction, containment and overlap signatures. Date/time truncation, extraction, time-zone conversion and descriptor-owned collations are explicit. JSON keys, patterns, array values and time-zone inputs bind as values. |
| Relations and sets | `InnerJoin/LeftJoin/CrossJoin` take explicit operands and conditions. Scoped aliases handle self-joins. `Lateral` gives its subquery or set-returning function access to listed left-hand occurrences. `Derived`, scalar subquery, `Exists/NotExists`, `InQuery` and `Union/UnionAll` preserve a declared row shape. Set operands must agree in column order and type. `DistinctOn` checks the PostgreSQL sort prefix. |
| Aggregates | `GroupBy`, `Having`, `CountAll/CountOf`, sum, average, minimum, maximum, ordered/distinct aggregates, boolean, string, array and JSON aggregation, and `FilterWhere` on an aggregate. `COUNT` yields non-null bigint. Other result types and nullability follow the selected overload, including widening of integer sums. Explicitly grouped projection or a known valid grouping dependency is required. |
| Windows and CTEs | `Over` takes an aggregate or window function, `PartitionBy`, ordered terms and a checked ROWS/RANGE/GROUPS frame with typed bounds. `With` and `WithRecursive` declare names and output fields before use. Recursive terms supply a compatible seed, union mode and step over a scoped self-reference. Materialization is an enum. A modifying CTE exports only its RETURNING fields and contributes its write effects even if its rows are never read. |
| Locks | `ForRows(strength, targets, wait)` represents UPDATE, NO KEY UPDATE, SHARE and KEY SHARE, with WAIT, SKIP LOCKED or NOWAIT. `OF` uses relation occurrences. Definition rejects illegal placements such as locking a grouped result, a set operation or the nullable side of an outer join. Transaction-level advisory locks take either one bigint or two integers, a shared/exclusive mode and blocking/try semantics. An advisory lock needs a live explicit transaction, also when acquired before any data query. Its key can be a typed expression. |
| Routines | A definition names schema, routine, ordered PostgreSQL argument types, result type/fields, cardinality and effect. `Call` yields a scalar, `RowsOf` a relation, optionally lateral or `WithOrdinality`. Unary or binary helpers are conveniences. An arbitrary-arity signature uses ordered typed formal handles, with `Arg(formal, expr)`; wrong owner, duplicate, missing and surplus arguments fail at definition. Explicit casts select the registered overload. A runtime choice selects a predeclared handle from a finite set. |
| Writes | `InsertInto` accepts typed values, batches or a SELECT of matching width/types. `UpdateOf` supports simultaneous assignments and FROM occurrences. `DeleteFrom` supports USING. A conflict target is a declared constraint or a structural index target, including an applicable partial-index predicate. `DoNothing` and `DoUpdate` expose the target and an `Excluded` occurrence only in their legal scopes. `Returning` takes a typed projection and a one/many cardinality contract. |

Constant expressions that must match a schema-defined partial index are a special case. Model that target by a declared constraint when possible. For a partial index, admit only a developer-declared, typed structural predicate whose canonical form is validated against the catalog at definition verification. A column IS NULL predicate is one structural form that needs no literal. If PostgreSQL cannot infer a different target with bound values, defer that plan until there is a matching typed structural form. Application values always bind. No constructor accepts predicate text or emits a caller-formatted literal.

For example, a filter on aggregate inputs uses `CountOf(id.Expr()).FilterWhere(Eq(state.Expr(), wanted.Expr()))`. A correlated subquery takes an explicit outer handle and may return a scalar only with one output column. PostgreSQL enforces the at-most-one-row contract during execution when it cannot be established from a unique key or an aggregate. Giving a scalar subquery an implicit LIMIT would change its meaning. Recursive terms, grouped expressions and frame restrictions use the common PostgreSQL 14–17 surface. [Table expressions](https://www.postgresql.org/docs/17/queries-table-expressions.html) · [CTEs](https://www.postgresql.org/docs/17/queries-with.html) · [SELECT](https://www.postgresql.org/docs/17/sql-select.html)

In a nested read, a CTE publishes its own named result fields. The outer query has no access to its private base aliases. This sketch returns the first job in each group. `jobID` makes the window order total within each group.

```go
ranked := q.NamedRows("ranked", rankedJobsView)
idOut := q.Output(rankedJobsView, "job_id", q.Int8())
posOut := q.Output(rankedJobsView, "position", q.Int8())
rank := q.Over(q.RowNumber(), q.Window().
    PartitionBy(jobGroup.Expr()).OrderBy(q.Asc(jobID.Expr())))
body := q.Select(q.Project(idOut, jobID.Expr()), q.Project(posOut, rank)).From(j)
r := q.FromNamed(ranked, "r")
first := q.Input("first", q.Int8())
read := q.With(q.As(ranked, body)).Select(
    q.Project(q.Output(firstJobsView, "job_id", q.Int8()), q.Ref(r, idOut)),
).From(r).Where(q.Eq(q.Ref(r, posOut), first.Expr()))
firstJob := q.DefineRead(firstJobsView, "first_job", read)
```

The caller binds `first` to one. The scope predicate lives inside `body`, so rows are filtered before ranking. A modifying CTE uses the same named-row mechanism with a typed `Returning` projection. Its writable owner must also be registered. A transaction advisory lock uses a registered signature and typed key slot, for example `q.AdvisoryXactLock(q.Exclusive, q.Wait, key.Expr())`. A two-integer key and the nonblocking boolean-returning variant have distinct overloads. Statement keys describe these operations without embedding the lock key. [PostgreSQL advisory lock functions](https://www.postgresql.org/docs/17/functions-admin.html#FUNCTIONS-ADVISORY-LOCKS)

## Compilation, binding and snapshots

1. **Freeze a shape.** Capture explicit source, projection and clause order in immutable nodes. Identifiers come from registered descriptors. Check and quote schema, relation, alias and column as separate components. Reject duplicate aliases, out-of-scope fields, untyped overloads and incompatible UNION/CTE rows. Reject a foreign handle even when its Go type or printed name matches.
2. **Resolve scope and effects.** `ByScopeColumn` ties an occurrence to the session scope. `ByRLS` requires an explicit descriptor declaration, an identified database policy and role-bound verification. Only a relation declared global can use `Global`. A query cannot relabel a scoped table as global. Pre-filter a scoped source at the first point preserving its row semantics. An inner source can contribute a top-level predicate. On the nullable side of a left join the constraint belongs inside the source or in ON. Apply the rule independently to aliases, correlated subqueries, set arms, CTEs and DML sources. Use one session scope slot at every occurrence. Opaque function bodies require a declared access/effect contract and database enforcement.
3. **Lower and bind.** Use fixed clause order, stable identifier quoting, explicit projection aliases and parentheses preserving the expression tree. Reserve the first slot for session scope when used. Allocate remaining slots on first use in a documented traversal of the emitted clauses, including nested trees, left to right. A repeated handle shares its slot. Count and page variants each receive dense bind plans. Runtime values, CASE alternatives, JSON paths, limits, offsets and cursor components never enter the SQL string. Map iteration, pointer addresses and registration interleavings cannot affect emitted bytes.
   A slot renders as `CAST($n AS "schema"."type")`. The cast establishes the parameter type even in a projection or in a comparison between two slots. Type schemas join the readiness requirements. A column-specific modifier, such as the length in `Varchar(n)`, belongs to catalog verification. The cast omits it to preserve PostgreSQL comparison and assignment semantics.

4. **Bound alternatives.** Independently optional filters may opt into a fixed SQL shape with a bound on/off boolean and typed value. The compiler preserves the distinction between an omitted filter and an enabled filter whose value is SQL NULL. A structural alternative uses a named finite choice when plan selection, joins, order, lock mode or side effects differ. No automatic boolean rewrite of a volatile expression. Enumerate the reachable shapes, register them all and fail definition on an exceeded declared budget. The default budget is 256 shapes per plan. Report the plan, contributing option names, their alternative counts, and the first combination exceeding the budget. Count only reachable combinations. A caller can split the plan or choose an explicit compact form. Compare EXPLAIN on representative inputs before moving a selective query to fixed guards.
5. **Register and execute.** Store every executable shape in the owning definition's `Statements` and `StatementQueries`, including count, each allowed ordering/direction and the first/after keyset pair. Use readable stable keys such as `groups_by_size.scoped.sort.id.asc.page`. Preserve existing C3 baseline, single-filter and all-filter verification where they expose different SQL. A bounded shape compiled once may serve many runtime combinations. Runtime resolves an existing key, binds and executes. No lazy registration.

A page takes an order handle, direction and typed bounded page parameters. Offset uses the plan's maximums. Keyset pairs a total, deterministic ordering with a unique final tie-breaker and a cursor of the same arity, type, collation and plan/order identity. Compare in the effective direction of each term. Use a tuple comparison only for compatible non-null terms in uniform direction. For mixed direction or explicit NULLS FIRST/LAST, emit the lexicographic OR ladder with null-aware equality and the same null ranking as ORDER BY. Reject unverifiable cursor orders at definition. Derive a count from the unpaged result after grouping, DISTINCT and UNION. A separate optimized count needs a declared equivalent shape and parity tests. [Existing pagination contract](DESIGN-v0.3.1.md)

Variable-size value lists can use one typed array slot with ANY or an explicitly declared unnest source. Fixed-width VALUES batches need a declared shape for each allowed width and must fit the PostgreSQL bind-parameter budget. Splitting a mutation can change conflict, RETURNING and statement-trigger semantics. A single-statement plan rejects a batch it cannot execute with its declared semantics.

## Authorization and existing tools

| Boundary | Integration contract |
| --- | --- |
| Registry and session | Bind through the existing registry and validate its session proof before reading scope or executing. Preserve callback-context checks, busy/closed errors, role selection, timeouts, cancellation and commit errors. A plan cannot install a role or GUC. Table read-only flags, `DenyDelete`, `DenyUpsert`, `DenyUnscopedWrites` and the runner's capabilities apply across all nodes. |
| Writes and scope | Inject or check session scope on every inserted row, INSERT SELECT projection and updated target. For scoped upserts require a conflict key that establishes the same scope or an explicitly verified equivalent constraint/policy. Keep key, scope, generated, once and version fields protected. Offer checked version/row-count operations and retain soft-delete visibility. Physical DELETE requires the table opt-in. Explicit unscoped access requires policy permission. |
| Method routing | Ordinary reads reuse Scan/Each and the existing observation pipeline. A row lock requires a writable explicit transaction. Transaction-level advisory locks require transaction lifetime and a permitted lock effect. `Direct` rejects those operations. Routine effects default to requiring write authorization until declared and verified read-only. A SELECT spelling is insufficient to classify its effects. Preserve PostgreSQL errors including 25006. |
| Typed write results | Single-row and affected-count writes reuse compatible Returning/Exec paths. For multiple returning rows or SELECT over a modifying CTE, the implementation needs a **write-authorized row stream** on both drivers. It must carry the trusted plan, check the outer command and every nested effect against the configured method policy, enforce `WITH` allowlists, and hold the session lease through cursor close. A driver lacking this capability returns an explicit unsupported error before I/O. Keep read guards strict and verify all branches on real PostgreSQL. |
| Privileges and role binding | Derive per-shape base relation/command access and column grants across projections, ON, predicates, grouping, ordering, CTEs, assignments and RETURNING. DML adds target INSERT/UPDATE columns, table DELETE, SELECT on read columns and sequence usage where needed. Record schema USAGE and exact function EXECUTE signatures. Count-star/table-existence reads can need any-column SELECT. Row locking needs UPDATE on at least one column of each locked table. Extend readiness with an any-column UPDATE check for that minimum grant. [SELECT privileges](https://www.postgresql.org/docs/17/sql-select.html) · [INSERT privileges](https://www.postgresql.org/docs/17/sql-insert.html) |
| sqlcheck | Preserve definition/key ownership on snapshot round trip. Add additive per-key metadata for access, privilege needs, effects and result types. Keep `CollectPrivilegeNeeds`' aggregate view. Select only shapes bound to a particular role, merge their needs with caller-supplied `ReadinessNeeds`, and feed `CheckReadiness`. Use structural access for `BindRolesWithDefinitions` and `CheckPolicyCoverage`. An invoker routine with unknown body access requires an explicit declaration. A reviewed definer routine can have zero caller-table access and still requires EXECUTE. Record its signature and verify security mode in the target catalog. Role grants, FORCE RLS and row visibility/denial tests remain independent checks. |
| Verification and observability | Extend `CollectSQL` to recognize typed declarations and execution owners. Retain explicit owner binding where source analysis cannot prove ownership. Every finite variant appears in role-bound PREPARE/EXPLAIN and output-schema checks. Execute parity cases on disposable databases, including RLS and null/empty results. Both drivers and registry observers use the registered statement key, quiet/secret flags, conservative secret propagation to slots, the existing diagnostics policy and low-cardinality metrics. No parameter values enter shape keys, tags or metrics. [Authorization checks](AUTHORIZATION.md) · [Logging](LOGGING.md) |

Role, permitted routine effects and definer signatures, scope variables, access to catalog relations and migration sequencing belong to the application. The compiler supplies descriptors and verification metadata for those choices. The session still applies its configured guards to every generated statement. Binding an argument to a configuration-reading routine must preserve any restriction on protected variable reads. A runner must explicitly permit WITH to execute a typed CTE.

## Compatibility and native SQL

**The new builder exposes no public Raw expression, clause, identifier, operator or statement constructor.** Even a registered arbitrary expression would hide types, relations, effects and parameter provenance from the compiler. A new SQL construct requires a typed node, a binding contract and database verification. The internal renderer owns its fixed SQL vocabulary.

The existing Table, Rows, View and QuerySpec APIs keep their default behavior. Set `RegistryConfig.Deny` to include `DenyLegacySQL` to reject fragment-based QuerySpec declarations, hand-written registered SQL and string filters. A strict runner or Direct pool receives an execution credential from the registry. A strict descriptor rejects a mismatched session before I/O. Direct SQL calls, including text identical to a compiled shape, cannot execute through that runner. Raw transaction entry is also unavailable. Separate runners can enforce different policies. A database handle acquired separately is outside the registry.

## Finite single-table shapes

Use the same compiler, variant keys and snapshot format for declared single-table operations. A declaration enumerates allowed assignment column sets, typed predicate alternatives, ordering, and scoped or unscoped binding modes. Include the data and count forms of a page and filtered aggregate shapes. Reject undeclared combinations before I/O in strict mode. Untyped string Filter callbacks remain legacy operations. Source inventory resolves each declared plan to its owner and expands its exact keys for role-bound PREPARE and EXPLAIN. An unresolved owner needs an explicit binding. Runtime SQL generation cannot silently extend the verified set.

## Compiler and adapter interfaces

`TableSpec.RelationScope` declares the enforcement for a physical table. A tagged scope column supplies the default for typed occurrences. A table without a scope tag can explicitly declare `ByRLS()` or `Global()`. The scope argument to `FromTable` must agree with that declaration. Column types are checked against the Go field and against declarations of the same physical column, including across aliases. `CatalogType[V]` accepts separate schema and type identifiers and an optional bind encoder.

A typed expression contributes a result type, nullability, read columns and ordered slots. The compiler includes each alternative under its own key in `Definition.Statements`, records its plan name in `StatementQueries` and keeps the verifier's per-key metadata in `StatementShapes`. Compilation validates a complete family before registering any member. Verification snapshots include physical column names and declared catalog types in stable order, column grants, inputs, result fields, physical RLS relations and effects.

The compiler derives schema and RLS requirements from the physical sources for reads and writes. A shape using column scoping must render the predicate for every such occurrence. Its unscoped variant records `UnscopedOnly` and requires an unscoped session when binding. Registration checks physical commands against each table's write mode. A typed read on an insert-only table and an update outside an update-only table's declared columns fail before the registry can issue a call.

The driver boundary consists of `SQLAuthority`, `AuthorizedStatement` and `AuthorizedSession`. At startup a registry supplies a verification credential. Its driver presents a call, its own session pointer and the intended method to `SQLAuthority.Open`. The successful result contains the registered SQL, its observation key, ordered arguments, redaction mask and effects. A shape referencing a secret column conservatively marks every argument for redaction. Execution still uses the driver's normal statement guards, transaction checks and observation settings.

## Release sequence and acceptance

| Version | PostgreSQL capabilities |
| --- | --- |
| v0.4.0 | Single-table typed relations, columns, slots, predicates and grouping. Find, List, Count, scalar/grouped aggregates, and predicate-based UpdateMany/DeleteMany. Finite assignment, ordering and page variants. Registry snapshots, per-role authorized execution and verified RLS-only scoping. |
| First subsequent patch | Explicit inner, left and cross joins, including self-joins, with per-occurrence scope and privilege verification. |
| Advanced read releases | Routine signatures, row and advisory lock effects, correlated and derived subqueries, LATERAL, set operations, read-only and recursive CTEs, windows and nested pagination. Publish by completed capability. |
| Advanced write releases | Typed INSERT, UPDATE and DELETE source queries, conflict handling, write-authorized RETURNING streams and modifying CTEs. Publish by completed capability. |

The read and mutation compilers share projection types, relation identity, deterministic binding, effect declarations and shape registration. A modifying CTE consumes a mutation with a declared RETURNING shape. Each release requires SQL snapshots, ordered parameter/type assertions and definition-error tests. On both drivers, verify row visibility, nulls, affected rows, ordering, locks and errors against PostgreSQL 14–17. Include repeated sort values, an outer join with an out-of-scope match, invoker and definer roles, missing grants and writes embedded in reads.
