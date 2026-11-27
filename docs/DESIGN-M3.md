# M3 design for Tabula v0.4.4

Status: approved design, based on published v0.4.3. Implementation and its acceptance matrix are in progress. Signatures below are the shared implementation contract. Existing public signatures, SQL for unchanged declarations, statement keys and default policies remain unchanged.

M3 composes queries and writes from typed nodes. It adds subqueries, derived relations, lateral dependencies, set operations, common tables, windows, typed insertion and mutation results. It also completes multiple row-lock targets and an explicit materialization path for repeated references to a volatile array value. [Examples](design/m3/examples.md), [backend contracts](design/m3/api-contracts.md), [acceptance matrix](design/m3/acceptance.md), [work packages](design/m3/work-packages.md) and [review decisions](design/m3/review-decisions.md) form one proposal.

## One compiler and one authorization path

The compiler gains lexical query blocks and a statement root. A block has local relation occurrences, exported result fields, explicit outer references, common-table bindings, expression phases, a result-key proof and an ordered parameter walk. A statement root owns the complete effect graph and the final result contract. Derived names are not physical relations and never create grants by themselves.

Every physical leaf retains its descriptor identity, exact catalog type, modifier, scope mode, liveness rule, write mode and issuer. Every expression retains SQL type, Go representation, effective nullability, codec, secrecy, native dependencies and stability. All blocks share one identity-based bind layout per final SQL shape. An input reused inside and outside a subquery has one parameter position. Independent declarations with the same spelling remain distinct. Runtime values are bound with typmod-free casts. Generated aliases are deterministic and cannot collide with declared aliases.

The registration unit stays a named plan, not an individual AST node. A complete family is checked and published atomically under the existing budget, default 256. Reusing a query fragment does not execute or separately register it. Optional filters, assignment sets and page forms are enumerated at the outer plan boundary. Nested executable `WithList` or `WithForms` families are rejected; nested queries have fixed structure and bound values. Budget errors name the contributing dimensions.

`DefineRead` continues to admit only read effects. New write declarations use `CallExec` or `CallReturningEach`, including when the outer SQL token is SELECT. A write inside a common table cannot acquire read authorization. `DenyLegacySQL`, session identity, callback context, busy/closed state, transaction limits, observation keys, Quiet and error privacy apply to the entire statement.

## Result fields without intermediate Go structs

M1 table mappings remain the source of physical fields. An intermediate relation does not need a second Go struct, decoder or registered View. Proposed exported-field handles are declaration objects:

```go
func Export[V any](name string, value Expr[V]) Exported[V]
func (f Exported[V]) Project() Projection
func (f Exported[V]) From(source QuerySource) Expr[V]
func Derived(alias string, query ReadSelect) QuerySource
func ResultOf[V any](source QuerySource, field OutputField[V]) Expr[V]
```

`QuerySource` is sealed and implements `ReadSource`. It carries an occurrence of a result schema. `Export` preserves the expression's type, codec, nullability and provenance. `From` requires that exact exported identity in the source's output schema; matching names alone are insufficient. `ResultOf` reuses a projection already declared for an existing output View. Only a final `DefineRead` or `DefineReturning` needs a Go result decoder. The internal `Projection` representation can accept an exported target at an intermediate block without changing the published `Project` signature.

The schema of a set result follows the first arm's ordered exports; later arms must match arity and exact SQL identities, with nullable outputs widened across arms. No implicit common-type selection or name-based field rebinding is allowed. Use an explicit existing `Cast` before the set operation. Reusing a derived relation under another alias creates a new occurrence of the same result schema. A left lateral or left ordinary join extends result-field nullability just as it does for physical columns.

`DefineTableRead` retains its current single mapped source contract. It can consume the new scalar/window expressions in legal clauses. A query with a genuinely different output shape uses `DefineRead`; the intermediate graph remains free of fake Views. Table-backed mutation results get a selection convenience using the existing `TableSelection` and partial decoder indexes.

## Subqueries and correlation

```go
func SelectValue[V any](value Expr[V]) ValueSelect[V]
func ScalarSubquery[V any](query ValueSelect[V]) Expr[V]
func ExistsQuery(query ReadSelect) Pred
func InQuery[V any](value Expr[V], query ValueSelect[V]) Pred
func AnyQuery[V any](op ComparisonOp, value Expr[V], query ValueSelect[V]) Pred
func AllQuery[V any](op ComparisonOp, value Expr[V], query ValueSelect[V]) Pred
func ArraySubquery[V any](query ValueSelect[V]) Expr[[]V]
func ArraySubqueryAs[R, V any](result PGType[R], query ValueSelect[V]) Expr[R]
func Outer[V any](value Expr[V]) Expr[V]
```

`ValueSelect[V]` is a one-column query with the same From, join, Where, GroupBy, Having, OrderBy and common-table vocabulary. It supplies one internal export and no View. `ComparisonOp` is a closed native set for equality, inequality and ordering; it is not an operator-name string. IN uses qualified native equality, and NOT IN is `Not(InQuery(...))`, retaining SQL NULL. ANY/ALL render a qualified operator and derive its exact EXECUTE requirement. Arity and type mismatches fail before registration.

`ScalarSubquery` is nullable because zero rows produce SQL NULL. More than one row keeps PostgreSQL `21000`; it is never silently limited. `ExistsQuery(Select().From(...).Where(...))` supplies a structural projection of 1 only when the SELECT has no explicit projection. It does not discard a projection that affects set, group, aggregate or distinct semantics. EXISTS produces non-null bool and provides no promise to evaluate all input rows. [Subquery expressions](https://www.postgresql.org/docs/14/functions-subquery.html)

`Outer(e)` moves the operand's name lookup exactly one lexical level outward. Nest it for a grandparent reference. Ordinary fields resolve locally; an unmarked foreign occurrence fails. An outer reference cannot escape the enclosing registry or use an invisible sibling, later join input or common-table body. Aggregate and window ownership uses the declared query block, not merely the lexical position of the function call. In particular an aggregate over outer-only fields must either be explicitly owned by the outer block or be rejected with guidance to export the aggregate first; the compiler must not silently move it between levels.

`ReadSelect.InnerLateral(source,on)`, `.LeftLateral(source,on)` and `.CrossLateral(source)` are explicit join constructors. A lateral source can reference only preceding inputs at that join point. A non-lateral Derived source must be uncorrelated. A lateral keyword with no outer dependency is still uncorrelated and may run once for the statement. Nullability, liveness and scope filters remain attached at the correct leaf/join boundary. [Lateral table expressions](https://www.postgresql.org/docs/14/queries-table-expressions.html#QUERIES-LATERAL)

Add `.Take(n)` for a nonnegative declaration-time bound in a nested query, represented by an internal bound input, and `.Limit(input,max)` for a caller-bound limit with a declared finite ceiling. Both preserve parameterization. They describe a nested SELECT rather than a new runtime SQL variant. A first-row query must explicitly choose an order or the existing unordered-first semantics. No arbitrary OFFSET or LIMIT strings are accepted.

## Array result sources and source-free projections

`Unnest` is a sealed result source, not a caller-named routine or a uniqueness declaration. The heterogeneous declaration uses exported identities:

```go
func UnnestColumn[A, V any](name string, array Expr[A], element PGType[V]) Exported[V]
func UnnestOrdinality(name string) Exported[int64]
type UnnestInput interface { /* sealed declaration */ }
func Unnest(alias string, first UnnestInput, rest ...UnnestInput) QuerySource
```

Only array-element exports and at most one ordinality export are valid UnnestInput values; an ordinary Export passed here fails definition. At least one array is required. The element catalog type must match exactly, including domain and enum identities. Array element codecs and nullable-element representations follow ArrayOf/ArrayOfNullable. Parallel arrays may have different element types and lengths. Shorter arrays, including NULL and empty arrays paired with longer ones, supply SQL NULL; output nullability includes that padding. No output column is presumed unique. WITH ORDINALITY adds an int8 starting at 1; it is a position within this invocation, not a persistent row identifier. Duplicate output names, duplicate ordinality declarations and incompatible elements fail before registration.

The compiler can render parallel expansion as `ROWS FROM (pg_catalog.unnest(...), ...)` to keep every function schema-qualified without relying on parser special treatment of an unqualified multi-argument call. This is equivalent to PostgreSQL's multi-array UNNEST, including NULL padding. Bind every array independently from its original Binding, record the exact native EXECUTE requirement and retain enclosing correlation and leaf grants. Correlated array sources use the explicit lateral/Outer boundary. The result can feed joins, EXISTS or a write source; matching uniqueness must be proved from a Derived GROUP BY/DISTINCT ON or other real result-key evidence. [Array expansion](https://www.postgresql.org/docs/14/functions-array.html) and [table functions](https://www.postgresql.org/docs/14/queries-table-expressions.html#QUERIES-TABLEFUNCTIONS) define the native semantics.

ArraySubquery constructs `ARRAY(SELECT ...)` from a one-column ValueSelect, with the same lexical correlation and OrderBy rules. Empty input produces an empty array, never SQL NULL. The shorthand yields []V and refuses an element that can be SQL NULL; ArraySubqueryAs permits an exact nullable-element representation such as ArrayOfNullable. This release admits scalar element types only; it does not silently treat a nested PostgreSQL array as a Go slice of unrelated dimensionality. SQL type, Go representation and element nullability are validated at registration, including outer-join extension.

Select with no From is legal. Self-contained inputs, scalar functions and nested subqueries need no invented physical relation or View at intermediate levels. The final output still uses its declared decoder. A plain source-free scalar projection yields one row; WHERE, aggregation and limits retain PostgreSQL semantics rather than an unconditional one-row promise. Unbound local fields and joins without an initial source fail. Every nested physical leaf contributes its exact grants, scope policy and effects. A source-free read remains EffectRead, including when it has no physical grants.

## Sets and common tables

`Union(a,b,rest...)`, `UnionAll`, `Intersect`, `IntersectAll`, `Except` and `ExceptAll` return `ReadSelect`. Preserve caller grouping with explicit parentheses; do not depend on textual operator precedence. Bag and duplicate-eliminating results keep their different multiplicities. All arms are effect-checked. A compound with a fixed order/page has an explicit outer result block; arm-local clauses stay inside their arms. A Derived wrapper exposes exports when callers want further predicates or expressions. [Set operations](https://www.postgresql.org/docs/14/queries-union.html)

```go
func CTE(name string, query ReadSelect, mode Materialization) CommonTable
func (c CommonTable) As(alias string) QuerySource
func (q ReadSelect) With(tables ...CommonTable) ReadSelect
func RecursiveCTE(name string, seed ReadSelect, combine RecursiveUnion,
    step func(self QuerySource) ReadSelect) CommonTable
```

Materialization has explicit default, materialized and not-materialized choices. Validate duplicate names, cycles and lexical dependencies at definition. Ordinary common tables may reference earlier common tables. A recursive self token is usable only in its own step; mutual recursion and repeated/self references in prohibited positions fail. Seed and step must have exactly compatible output SQL identities and Go codecs, with nullability computed conservatively. Recursion supports UNION and UNION ALL with explicit termination predicates, statement timeouts and cancellation. There is no client loop or hidden recursive query execution. SQL SEARCH/CYCLE decorators and projection of native record/record-array paths are explicitly outside this draft's public surface; bounded recursion and typed cycle predicates remain expressible. This boundary is approved for this release.

Common tables propagate physical grants and effects; they do not grant table ownership, establish a new RLS boundary or imply a unique result. No implicit automatic materialization is promised by the default mode. Explicit materialization is used when repeated evaluation matters. A not-materialized request cannot be used as a proof that a volatile expression is repeatable. [Common tables](https://www.postgresql.org/docs/14/queries-with.html)

## Window expressions

Add `WindowSpec` with typed partition expressions, existing RowTerm order, a closed frame declaration and optional named-window reuse. `Over(function, spec)` returns a typed expression. Ranking functions include RowNumber, Rank, DenseRank, PercentRank, CumeDist and NTile. Value functions include Lag, Lead, FirstValue, LastValue and NthValue, with explicit typed offsets/defaults. Existing ordinary aggregates can be wrapped in Over, including their legal FILTER forms. PostgreSQL 14 does not support DISTINCT or aggregate-local ORDER BY inside a window aggregate; reject these combinations instead of emitting invalid SQL.

Frames cover ROWS, RANGE and GROUPS, unbounded/current/typed preceding/following bounds, and EXCLUDE NO OTHERS/CURRENT ROW/GROUP/TIES. Bound legality, ordering arity and native offset types are checked at definition; dynamic negative offsets keep a documented pre-I/O validation or native error. RANGE offsets select only verified native order/offset combinations. No custom in-range function is inferred from a caller string. The default frame is PostgreSQL's default, not silently changed to ROWS. The API offers an explicit whole-partition frame and an explicit running ROWS frame for readability.

Window expressions are legal in projection and final ordering. They are illegal in WHERE, HAVING, GROUP BY, aggregate arguments, frame bounds and another window at the same level. Filter a window result through Derived. Ordinary aggregation may feed the subsequent window phase when PostgreSQL permits it. Window nullability includes empty frames and missing lag/lead rows. Ranking results retain their native int8/float8 identities. RESPECT NULLS is the PostgreSQL behavior; do not expose unsupported IGNORE NULLS. [Window functions](https://www.postgresql.org/docs/14/functions-window.html) · [Window syntax](https://www.postgresql.org/docs/14/sql-expressions.html#SYNTAX-WINDOW-FUNCTIONS)

## Typed writes and result streams

Use sealed non-executable write builders: `InsertInto(source)`, `UpdateOf(source)` and `DeleteFrom(source)`. They produce a `WriteStatement`, checked against the same physical declaration as the existing mutation API. Declare fixed ordered targets and values with Assign; there is no map whose iteration order changes SQL. `DefaultOf(field)` is a separate target assignment for database DEFAULT, not a generic expression. UPDATE requires a typed fixed predicate; intentionally affecting every authorized/live row uses an explicit `AllRows()` marker. The existing predicate-based mutation methods and their error behavior are unchanged.

Proposed registration and execution vocabulary:

```go
func DefineWrite[T, S any](owner RowSource[T, S], name string,
    query WriteStatement, observe ...StatementOption) *WritePlan[S]
func WithReturning(query WriteStatement, fields ...Projection) ResultStatement
func DefineReturning[T, S any](out *View[T, S], name string,
    query ResultStatement, observe ...StatementOption) *ReturningPlan[T, S]
// Each declaration above also has a Try-prefixed partner.
func (p *WritePlan[S]) Bind(s Session[S]) *BoundWritePlan[S]
func (b *BoundWritePlan[S]) Exec(ctx context.Context, values ...Binding) (int64, error)
func (p *ReturningPlan[T, S]) Bind(s Session[S]) *BoundReturningPlan[T, S]
func (b *BoundReturningPlan[T, S]) All(ctx context.Context, values ...Binding) ([]T, error)
func (b *BoundReturningPlan[T, S]) Iter(ctx context.Context, values ...Binding) iter.Seq2[T, error]
```

The explicit owner of DefineWrite must be the outer physical target. It supplies the definition/observation namespace. Returning projections determine their output owner. ResultStatement is sealed: a write with RETURNING or a SELECT containing an actual modifying common table can implement it. A read-only SELECT cannot be registered as a write to evade read policy.

Add free `DefineUpdateReturning(source,out,name,spec,sets,projections...)` and `DefineDeleteReturning(source,out,name,spec,projections...)`, with Try partners, for the finite MutationSpec/RowSet families. `sets` is a slice so the projection list is the only variadic parameter. Bound helpers keep existing set/filter choice arguments but return typed rows or a write-authorized iterator. They compile a new named family; they never mutate a registered MutationPlan or rerun a count-only update and then fetch rows. A table-selection convenience reuses MapTable and the partial decoder instead of Project for unchanged target row types.

No arbitrary multirow write gets a read-style LIMIT 2 One or LIMIT 1 First shortcut. Such rewriting would change which rows are modified. All/Iter consume the one declared write; caller early break is a result-consumption choice, not a promise to cancel its effects. Iterator closure drains or cancels according to the driver/transaction contract, reports failures, and releases the busy state. An execution error or scan failure must roll back the operation before the session can be committed if partial effects are possible. An explicit operation savepoint is permitted and documented for batched execution or client-side result failure; it must restore managed role/scope state and preserve the original error plus cleanup errors. A future exact-cardinality write helper would need this same rollback contract.

### Insertion and conflicts

InsertInto supports one VALUES row, a fixed declared set of rows, DEFAULT VALUES, and `FromSelect` with an ordered target column list. The SELECT exports supply values; arity, exact SQL type and nullability are checked before registration. Omitted serial/identity/default/generated columns remain omitted or DEFAULT according to their declaration. Inspect native default/identity/sequence metadata during readiness where it is a permission premise; do not infer arbitrary default-expression privileges by text heuristics.

A batch plan declares a finite list of row-count forms such as 1/16/128. Each form is registered and checked, including repeated inputs, scope slots and the 65,535 parameter ceiling. ExecBatch chooses only declared forms, validates every original row binding before its first I/O and executes inside one operation savepoint. It never reuses driver-adapted arguments between rows or shapes. All-or-error batch semantics include earlier chunks. Returning batches preserve no positional association with input rows: conflicts and triggers can remove rows, and PostgreSQL does not promise RETURNING order. Applications can include an explicit input key in the result when association is needed.

Conflict choices are explicit DoNothing or DoUpdate, with either a typed inference target or a declared constraint name. `Excluded(field)` is valid only within the conflict arm of that exact target. DoUpdate needs INSERT and UPDATE permission, plus SELECT for actual target/excluded/inference predicate reads. Targetless DoNothing retains the minimal INSERT-only path. WriteInsertOnly refuses all RETURNING and DoUpdate; WriteUpdateOnly refuses insertion. Once/key/scope/version protections also apply to conflict assignments.

Partial indexes need a special declaration boundary. An inference predicate with a runtime bind can fail in a forced generic plan. Propose a sealed `IndexPredicate` family with column null/bool tests, boolean composition and typed `IndexEq/Ne/Lt/Le/Gt/Ge(field, schemaValue)` comparisons. Values here are schema facts fixed at declaration, verified against the existing catalog index; no Slot, secret value or runtime filtering input is accepted. They are escaped by a closed literal renderer used only in the index-inference grammar. Ordinary predicates continue to bind every value. No public Raw or general literal Expr is added. This narrowly scoped exception is approved for the index-inference grammar only. Constraint names and expression-index operands still need catalog verification; unsupported custom opclasses/collations fail. DoUpdate forbids deferrable/exclusion arbiters and verifies an eligible immediate unique arbiter. Keep PostgreSQL conflict cardinality errors and conditional-update zero-row behavior. [INSERT and conflict inference](https://www.postgresql.org/docs/14/sql-insert.html)

Use PostgreSQL's inference result from EXPLAIN FORMAT JSON to identify actual conflict-arbiter indexes, then inspect their catalog validity, uniqueness and constraint properties. Do not compare deparsed predicate strings or build a second partial-index theorem prover. The schema-value renderer accepts only reviewed immutable type/operation pairs, rejects secret fields, and handles quoting without session-dependent backslash interpretation. No public constructor produces an arbitrary SQL fragment from these values.

### Update sources, delete sources and descriptor guards

UpdateOf may add typed FROM sources and matching conditions; DeleteFrom may add USING sources. Require a proof that each additional match supplies at most one value tuple per target when its values contribute to assignment or RETURNING. The initial proof accepts equality covering an immediate, non-null unique key, or a previously proved unique result key. Arbitrary one-to-many matches fail with guidance to an aggregate, a deterministic first-row subquery or EXISTS. A predicate-only Delete USING can instead be lowered to existence semantics when that does not change returned expressions. No declaration flag asserts uniqueness without catalog/result proof. [UPDATE matching](https://www.postgresql.org/docs/14/sql-update.html#SQL-UPDATE-NOTES)

All assignments are evaluated on the old UPDATE row; RETURNING reads the new UPDATE row and the deleted DELETE row. Joined result references retain the proven matching source. Scalar subqueries in assignments preserve the zero-row NULL and many-row error contract. Protected once, key and scope columns cannot be updated. Versioned operations use the existing ExpectVersion plus automatic increment; insertion uses the declared initializer/default. Soft deletion stays a typed UPDATE, while physical DeleteFrom requires AllowDelete. The current exclusion of already-deleted rows remains. A new write form must not add SELECT merely because a column is assigned. [DELETE results](https://www.postgresql.org/docs/14/sql-delete.html)

ByScopeColumn inserts automatically supply the session scope and exclude a caller assignment to that target. An unscoped insert into such a source requires a separately declared UnscopedOnly form with an explicit typed scope value; there is no fallback to a zero scope. ByRLS never injects a predicate or slot, including for INSERT SELECT, conflict arms, recursive leaves and modifying common tables. It records the actual physical INSERT/SELECT/UPDATE/DELETE policy needs. Global has no tenant behavior. RLS policy truth and trigger behavior remain database responsibilities verified with execution tests, not claims inferred from PREPARE.

## Modifying common tables and method policy

`WriteCTE(name, writeReturning)` exports the returned schema as a CommonTable. A result-less modifying common table can be declared as an explicit effect-only node and cannot be used as a FROM source. Only the top statement owns modifying WITH clauses. Reject nesting a write CTE in a scalar/derived/recursive read position. The final shape records the union of physical commands, grants, RLS checks and effects, including unreferenced modifying CTEs.

Use returned rows for dependencies. Sibling writes are not a sequential program and do not see one another's base-table changes. Conservatively reject multiple modifying branches targeting the same physical table until disjointness can be proved; different aliases are not evidence of disjoint rows. Final count/rows are the outer statement's result, not an invented sum of all modifications. No Page/List/Count helper may execute a modifying query twice. [Data-modifying common tables](https://www.postgresql.org/docs/14/queries-with.html#QUERIES-WITH-MODIFYING)

Add two opt-in policy fields, proposed `StatementPolicy.AllowAuthorizedWriteCTE` and `AllowAuthorizedWriteRoutine`. Defaults false retain the current command guard. They apply only after SQLAuthority validates an immutable compiled call, and only on the write-authorized path. The CTE option permits a SELECT result around signed DML CTEs while checking every DML command against the configured write allowlist and requiring WITH permission when policy is active. The routine option permits only exact signed EffectWrite routine shapes with verified signature/EXECUTE requirements. They do not admit direct SQL, caller-crafted AuthorizedSQL, another issuer, or a read-method call. The flags explicitly opt out of the outer-token/outer-RETURNING test for these signed forms only; lexical single-statement and session-escape guards still run. ReadOnly and Direct reject both forms before I/O regardless of these flags.

## Multiple locks and explicit single evaluation

Add `ReadSelect.Locks(first RowLock, rest ...RowLock)` to declare one ordered set of targets. The old Lock method remains a single-lock replacement setter. Reject duplicate occurrence targets in Locks, avoiding an implicit strongest-mode merge. Require each target to be a visible physical occurrence at that block, preserved through outer joins and eligible for row locks. Infer SELECT, AnyColumnUpdate and both applicable policy accesses per target. Do not push a lock through an aggregate, set result, window, CTE or nullable join side. A caller can lock an eligible inner physical read explicitly before deriving its result. An exact count or paired List on a locking plan remains forbidden.

Generate every lock clause in declaration order, with explicit OF and its own wait mode, in one statement. The read-only result descriptor verifier must not strip lock-looking tokens with regular expressions. A protocol descriptor avoids executing locks entirely; any read-only projection rewrite must use structural metadata and leave original PREPARE/EXPLAIN intact. [Multiple locking clauses](https://www.postgresql.org/docs/14/sql-select.html#SQL-FOR-UPDATE-SHARE)

For a volatile array first operand, retain the current direct NullIf rejection until the value has an explicit materialized result slot. Callers can export the array in a Materialized CTE or a correlated materialized lateral source, then pass its output field to NullIf. Repeated reads of that field refer to the same value in the enclosing result row. Do not label the value globally IMMUTABLE: provenance still records volatility for cursor and DISTINCT ON proof. Evaluation repeatability within a row and repeatability across a query traversal are separate facts.

This is an explicit relational boundary, not a silent scalar-subquery rewrite. An uncorrelated lateral cannot be used to promise per-row generation. The compiler requires a real declared outer dependency for that promise. UPDATE may use the materialized value through a uniquely matched FROM source; all keys and other reads are explicit and carry their own grants. A target with no permitted SELECT column is not silently widened to support such a rewrite. A direct, context-free volatile array NullIf remains rejected with a diagnostic pointing to the explicit result-slot form. No sequencing promise is added for unused branches of CASE or rows eliminated at another query phase.

### Existing shared advisory locks and network reads

AdvisoryXactLock and AdvisoryXactLockPair already support Shared as well as Exclusive, with Wait/Try overloads. Preserve their public API and exact native signature requirements. Shared transaction locks remain legal in a read-only transaction; Direct rejects transaction locks. The acceptance matrix explicitly rechecks shared/shared compatibility, shared/exclusive contention and release on commit/rollback. An ordered multi-key advisory statement is not added here: a SELECT list does not by itself guarantee the requested lock-acquisition order.

pgtype.Inet() and pgtype.InetPrefix() already provide native inet mappings. Add `Host[V any](value Expr[V]) Expr[string]` for the exact inet SQL identity, independently of the Go representation, using `pg_catalog.host(inet)` with its exact EXECUTE requirement. Preserve SQL NULL, classify native stability from the catalog, reject unsupported types and check both network mappings through both drivers. No new raw address or function-name input is accepted.

## Pagination, proofs and snapshots across query boundaries

M2 Page/List/Count operate on the final result block. Put seek after group/set/window/winner computation so it cannot change a winner, window partition or recursive step. Retain one original Binding snapshot and independently adapt each selected statement. Count preserves the semantic unpaged result and removes only expressions proven irrelevant to cardinality. No optimizer based on rendered SQL text is introduced.

Derived/common-table projections can preserve a base key only through an injective row mapping with all key fields exported and no multiplicity change. A join uses the existing conservative product key. Duplicate-eliminating set results can use the entire comparable output tuple. UNION ALL cannot invent a cross-arm key; require an explicitly projected, distinct per-arm discriminator plus each arm's proved key, or reject keyset registration. Recursive results, row-number values and a declaration's apparent uniqueness are not automatic key proofs. Window-derived sort values need deterministic partition ordering and the existing representative/collation proofs; where no proof exists, Page with bounded offset is still available and keyset fails before registration.

Snapshot metadata adds only cloneable structural facts needed for column lineage, result-key premises, conflict arbiters, evaluation boundaries and effect edges. Do not persist server-local OIDs, query values or hidden native parameter adapters. Logical identities and shape keys remain stable. Verify exact parameter OIDs, physical OIDs/modifiers, named constraints, native function signatures, deterministic collations and RLS/FORCE. A newer metadata feature requires a verifier that knows it; an old checker must not be reported as full validation.

`DefaultRequirement.IncludesOmitted` marks an INSERT and `InsertColumns` records its actual target list. The verifier takes the catalog complement, including unmapped physical columns, and unions explicit DEFAULT columns. An empty target list represents DEFAULT VALUES. UPDATE DEFAULT retains only its explicit column list. Resolve sequence dependencies through the active `pg_attrdef` dependency edges, rather than sequence ownership or parsed default text; identity columns do not require caller sequence USAGE. Keep arbitrary default-function execution tests separate.

## Safe result description for writes

The current database/sql verifier obtains SELECT result OIDs with a zero-row read wrapper. It cannot wrap DML RETURNING as a subquery. More importantly, putting LIMIT 0 on a modifying CTE still runs the modification. Rolling it back does not undo every possible sequence or function effect. M3 must never use that technique to describe write results.

Propose an additive `sqlcheck.Verifier` backend contract with a transaction-scoped parse/describe capability, plus `VerifyRoleStatementsUsing`/`VerifyStatementsUsing` entry points. Keep existing database/sql functions and their read behavior. A backend operates role setup, catalog checks, PREPARE/EXPLAIN, parameter/result description and rollback on the same connection; do not splice descriptors from a different server or snapshot. Its small standard-library-only types expose parameter OIDs and ordered name/type OID/type-modifier fields, plus normal Exec/Scan/Each operations. The execution plan sees none of these verification credentials.

The pgxdriver module supplies a protocol backend using Parse + Describe + Close without Execute for the statement being described. Applications using stdsql/lib/pq at runtime can choose this separate validation backend. database/sql does not expose a portable Describe operation, and lib/pq's public API does not expose the prepared result descriptor; no reflection into driver internals or new core protocol client is proposed. Existing database/sql-only verification of a new write-result shape returns a clear `ErrDescribeRequired`, rather than silently dropping result checking or executing DML. Plain count-only writes keep PREPARE/EXPLAIN validation. The CLI should expose the missing capability clearly and document the native verification path; adding native driver imports to core is not acceptable. This dependency boundary is approved.

Use real protocol result fields to validate names and wire OIDs; nullability remains the compiler's contract because RowDescription does not supply it. PostgreSQL also flattens a domain result to its base type and modifier in RowDescription. Keep logical domain/enum lineage separate: verify the physical domain and parameter identities through catalogs, compare the expected wire base identity, and use result-origin evidence where present. A computed domain result whose logical identity cannot be established independently must fail explicitly; a matching base OID alone is not proof of that domain. Read-only exact-type probes remain available for actual reads. [PostgreSQL domain description implementation](https://raw.githubusercontent.com/postgres/postgres/REL_14_STABLE/src/backend/access/common/printtup.c)

Grant checks still require EXPLAIN and execution acceptance; Describe alone does not prove RLS or runtime sequence access. Test that neither a sequence nor a write function is invoked by write-result description. [Extended-query Describe](https://www.postgresql.org/docs/14/protocol-flow.html#PROTOCOL-FLOW-EXT-QUERY)

## Compatibility and review checkpoints

No existing declaration changes SQL merely because M3 is linked. All new builders are immutable values and produce no I/O until execution or explicit verification. The old raw escape surface remains governed by DenyLegacySQL; no new Raw, unchecked query string or generic public literal is introduced. Core and pgtype remain standard-library-only. Go examples use free generic functions where Go cannot express a method with new type parameters.

The proposal is complete enough to freeze shared compiler interfaces after decisions on the verification backend, the index-only schema-value boundary, explicit materialization, and recursive decorations. All four decisions are approved; implementation proceeds under the shared contract. The acceptance document separates executable SQL reference probes from future compiler and driver tests; neither reference SQL nor a design snippet is evidence of a shipped Go API.
