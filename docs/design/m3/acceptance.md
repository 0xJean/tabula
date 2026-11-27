# M3 acceptance and evidence

This is the required implementation matrix. Except for the separately recorded SQL reference probes, the tests below are planned and have not run. Design examples are not compiler verification. Implementation starts from the approved shared contract; only completed tests count as evidence.

## Matrix and final gate

- Execute each SQL-generation, authorization and result-codec case with stdsql/lib/pq and pgxdriver on disposable PostgreSQL 14, 15, 16 and 17. Use current installed patch versions, recording exact server and Go versions in the evidence.
- CI remains two supported Go releases across four PostgreSQL versions, with both runtime drivers in the integration suite, plus four quality jobs: 12 successful jobs on the exact candidate head.
- Add explicit root, pgxdriver, oteltabula, benchmarks and dedicated integration-package lint runs, without scoped suppression. Run all integration tests with race detection and fail when the disposable DSN is missing.
- scripts/workspace.sh and scripts/check.sh --all, generated reference, compile tests, six fuzz targets, size limits, core dependency boundary and snapshot compatibility all pass. New guard/lexer behavior carries bypass regressions and fuzz seeds.
- Full diff, messages, branch, proposed/actual PR and release text pass the repository's local neutrality scanner. Private vocabulary stays outside the repository. No platform secret or new CI configuration is required.
- Release preparation uses the final committed root tree; all three child dependency manifests/checksums, README/changelog and release verify align. Final audit precedes merge/tags. After merge, main CI, three tags and empty-cache public proxy plus checksum verification are required.

## Query and result cases

| Area | Positive cases | Rejection / regression cases |
| --- | --- | --- |
| Lexical scope | Correlated predicates, nested Outer levels, repeated same slot across blocks, independent alias reuse in set arms | Unmarked outer field; sibling/future alias; foreign issuer; escaped recursive token; duplicate local alias; aggregate accidentally bound to wrong level |
| Result exports | Scalar/nullable exports, MapTable fields, derived self joins, reordered partial projections, domain/enum codecs | Same text with wrong export identity; wrong Go mapping/OID; missing derived field; hidden output widened into wrong decoder |
| UNNEST | Single/multiple arrays, heterogeneous types, NULL/empty arrays, unequal-length NULL padding, nullable elements, int8 ordinality, EXISTS/JOIN/write-source use | Duplicate names/ordinality, no arrays, ordinary export in array source, element mismatch, fabricated unique key, implicit outer capture |
| ARRAY subquery | Ordered/correlated scalar elements, empty result is empty array, nullable elements through ArraySubqueryAs | Wrong element SQL identity/Go representation, nullable element into []V, unsupported multidimensional output |
| Source-free SELECT | Inputs/functions, independent and correlated scalar/EXISTS summaries, zero physical grants, nested RLS and scope propagation | Undeclared local field, JOIN without a source, hidden write effect, accidental widened grants |
| Scalar query | One row; zero row NULL; ordered Take; bound limit ceiling; aggregate over empty input | Multiple rows preserves 21000; extra column; wrong type; negative/excess limit before I/O |
| EXISTS and quantifiers | Empty and nonempty sets; NULL lhs/rhs; empty ANY=false/ALL=true; IN/NOT IN truth tables; qualified native comparisons | Fake one-row scalar result, wrong arity, unsupported operator, shadowed search_path operator, discarded group/set semantics |
| LATERAL | Inner/left/cross; preceding-input references; empty right result; nested outer references | Missing declared correlation for per-row promise; forward reference; nullable result into non-null output; non-lateral correlation |
| Sets | Six UNION/INTERSECT/EXCEPT variants, duplicate and NULL multiplicity, branch-local order/limit, nested precedence | Arity/type/codec mismatch, implicit narrowing, hidden row keys changing DISTINCT, side-effect branch accepted as read |
| Common tables | Repeated references, materialization choices, earlier binding references, stable shape keys | Duplicate names, undeclared/forward dependency, impossible cycle, fabricated uniqueness, volatile node classified as immutable |
| Recursion | Empty seed, bounded growth, UNION deduplication, UNION ALL multiplicity, cancellation, scope on every scan | Incompatible seed/step, mutual recursion, illegal repeated self reference, prohibited aggregates/window use, cross-registry seed |
| Windows | Ranking peers, lag/lead defaults, first/last/nth frame behavior, aggregate OVER, FILTER, named window reuse | Window in predicate/group/aggregate argument; nested window; DISTINCT/local aggregate ORDER BY unsupported by PG14; invalid frame ordering |
| Frames | ROWS/RANGE/GROUPS, NULL order, mixed direction, whole/running frames, exclusions, valid integer/interval offsets | Negative bound, wrong native offset type, RANGE with wrong order arity, nonnative in-range support assumed without proof |
| Lists | Count after set/group/window filters, empty input, outer NULLs, derived keysets, UNION ALL discriminators, independent pair layouts | Seek pushed below window/winner/recursion; fabricated key; volatile representative; second execution of write CTE; unresolved collation |
| Locks | All strength/wait forms across at least two aliases, NOWAIT conflict, SKIP LOCKED, savepoint/transaction release | Duplicate target, nullable side, aggregate/window/set target, missing UPDATE on one alias, count/list, read-only/Direct |

## Write and permission cases

| Area | Required cases and observations |
| --- | --- |
| VALUES and defaults | Explicit/omitted/default/serial/identity/generated columns; narrow mapped source omitting a physical serial or identity column; serial ownership with a constant replacement default; unknown target; duplicate target; correct UPDATE-only/INSERT-only failures before I/O; no wide SELECT inference. |
| INSERT SELECT | Zero/one/many input rows; exact output-target types; nullable input; SELECT source plus INSERT target grants; ByRLS no scope slots; ByScopeColumn target generated once per row. |
| Conflicts | Targetless DoNothing with INSERT-only grants; selected columns/constraint; EXCLUDED reads; conditional DoUpdate; duplicate proposed rows; partial predicates under force_generic_plan; revoked predicate-column SELECT; invalid/deferrable/exclusion arbiter. |
| Assignment and matching | Literal/bound/column/CASE/subquery values; UPDATE-only target with no SELECT on overwritten column; RHS-only SELECT; unique FROM proof; ambiguous multi-match refusal; DELETE predicate-only existence; joined RETURNING with and without matching proof. |
| Protected columns | Key/once/scope/version, serial generation and sequence grants; version increment/check; soft delete exclusion; explicit physical deletion; unscoped insert requiring its own declared scope form. |
| Returning | Correct old/new target semantics, nullable fields, joined columns, triggers changing values/suppressing rows, zero/many rows, permission failures; no positional association claim for batch results. |
| Operation failure | Scan/decoder error, binding failure, callback error, cancellation, iterator break/stop, Close failure, savepoint cleanup and role restoration. Verify rollback of partial batch/client failure and that reusing the session has the documented outcome. |
| Batch | Declared sizes including remainder, zero-row short circuit, parameter limit, same Binding reused across shapes and chunks, arrays/JSON/UUID/NULL, input slice mutation after binding, chunk failure and rollback. |
| Write CTE | Changed rows feeding another table; outer SELECT returning rows; result-less effect node; unreferenced write still has EffectWrite; SELECT snapshot visibility; simultaneous same-table targets rejected; final outer count semantics. |
| Authorization | CallReturningEach for any write result, matching issuer/session/method, DenyLegacySQL rejection even for copied text, closed/busy/callback context; Direct and read-only fail closed. |
| Method policy | Defaults preserve prior checks; separately enabled signed CTE/routine paths; all physical write verbs allowlisted; direct SQL and forged view never use the option; lexical/session guards still reject escapes. |
| Policy/readiness | Minimum grants pass; revoke each INSERT/UPDATE/DELETE/SELECT/sequence/native EXECUTE item and obtain exact findings; RLS+FORCE for leaves; role-bound PREPARE/EXPLAIN for every shape. |

## Evaluation and verifier cases

1. Direct volatile-array NullIf stays an atomic TryDefine failure. Materialized result-field form produces one generated array per selected row, references it twice without a second generator call and preserves SQL NULL/empty arrays. Include a real correlated lateral case and an uncorrelated negative control. Ordinary output remains permitted while keyset/winner proof rejects volatile provenance. No new physical read is silently added.
2. Run plain DML RETURNING and data-modifying CTE descriptions without executing the statement. Include a sequence and a write routine that would reveal accidental execution. Parse/Describe/Close leaves their state unchanged, and the verifier connection is rolled back/closed on every error.
3. A database/sql-only backend facing a new write-result shape returns ErrDescribeRequired. It must not execute a rollback-only probe, assume declared result OIDs are server evidence, or call the read LIMIT 0 wrapper. Existing read and no-result write verification remains compatible.
4. Protocol backend checks parameter OIDs and ordered result names/wire OIDs on the same connection and role as catalogs and EXPLAIN. Include domains, enums, arrays and nested result exports. Explicitly test domain flattening in RowDescription, physical domain/origin verification, mismatched logical domains with the same base type, and rejection when computed logical identity lacks proof. Incorrect metadata fails without row execution. Role reset, statement cleanup and context cancellation are tested.
5. Independent of runtime driver, actual write executions in disposable tests decode every result and verify affected data. Describe does not replace runtime sequence, trigger, RLS or function tests.
6. Catalog write prerequisites preserve source-snapshot ownership. Omitted INSERT columns are discovered from the physical catalog, UPDATE DEFAULT uses its exact list, and actual generic-plan conflict arbiters must match the declared target. Failed capability or savepoint cleanup stops the verification workload and closes its transaction; cancellation cannot suppress bounded rollback.

## Inventory, compatibility and privacy

- CollectSQL recognizes all new Define/TryDefine roots, intermediate exports and fixed query graphs. It binds the final plan's complete finite family to its owner, never counts a fragment as an independently executed statement, and preserves explicit Access/NoAccess rules for routine bodies.
- Registration is atomic on any bad nested branch. Default budget counts all scopes, choices and batches. New metadata is cloned deeply; editing Definition or a snapshot cannot authorize a new statement.
- Parameter normalization happens once at BindValue; every invocation/shape adapts original canonical values once. Exercise paired lists, reused CTE slots, subqueries, batch and returning paths with custom encoders that count calls.
- Secret propagation follows physical leaves, projection targets and nested expressions. Trace keys, metrics and snapshots contain no values. RawErrorDiagnostics remains an explicit existing opt-in; default errors retain class and SQLSTATE only.
- Unchanged v0.4.3 declarations retain SQL, keys, result ordering and budgets. New features require regenerated snapshots and an upgraded verifier. Public API comparison reports additive changes and any positional-literal considerations.

## Reference probe status

`reference-cases.sql` exercises PostgreSQL semantics independently of the proposed Go API: materialization and correlation counts, per-target locks, ROWS frames, scalar cardinality/NULL, quantified comparisons, bounded recursion, set multiplicity, modifying CTE under LIMIT 0, and partial-index inference under generic planning.

The design reference probes passed on disposable PostgreSQL 14.25, 15.19, 16.14 and 17.12. Their raw logs remain outside the repository. These probes are design evidence only. The compiler, protocol backend and complete dual-driver acceptance matrix remain unimplemented.

## Existing-family acceptance additions

- Shared advisory: both bigint and int4-pair overloads, Wait/Try, two concurrent shared holders, conflicting exclusive holder, read-only transaction success, Direct refusal, commit/rollback release and exact EXECUTE revocation. Preserve all existing APIs; no unproven acquisition order.
- Network Host: Inet and InetPrefix, IPv4/IPv6 and prefix removal, SQL NULL, unsupported type rejection, exact native EXECUTE revocation, output text OID and both driver decoders.
- Multi-array UNNEST must not manufacture a unique matching flag for UPDATE FROM. Prove a Derived grouped/distinct result key, and reject a duplicate value-bearing match without that proof.
