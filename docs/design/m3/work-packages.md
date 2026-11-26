# M3 work packages and estimate

The design decisions and additive scope are approved. Freeze the shared contract and proceed with implementation. Use three prepared worktrees after the shared contract has a reviewed local commit. The integration owner retains all publication work. No worker merges to main or creates tags.

## Shared contract before parallel work

Freeze the following interfaces together:

1. A lexical query-block environment with local/outer lookup, sealed exported fields, QuerySource result identity, explicit correlation and common-table bindings. Preserve old physical occurrence paths.
2. A relational result descriptor: ordered outputs, type/nullability/codec/secrecy, multiplicity/key evidence, effect and physical-leaf lineage, row-local materialization repeatability distinct from traversal stability.
3. A statement root with one parameter walk, final result contract, exact physical commands, effects and authorized call method. The renderer emits shape metadata and SQL together, without later string inference.
4. WriteStatement and ResultStatement sealed nodes; no executor or driver behavior in expression nodes. Modifying CTEs are top-level-owned effects even if not referenced.
5. Verification backend lifecycle and result description fields, plus policy hooks that consume only a validated signed call. Do not add a database driver to core.
6. Result-projection hooks that support intermediate Exported fields while preserving existing View/Table decoders and all v0.4.3 snapshot paths.
7. Reusable nullability, grouping, native signature, catalog-key, scope, liveness and budget fixtures. Each ownership boundary gets compile/unit tests before parallel changes.

## A — query composition

Own new exported/result-source nodes, lexical correlation, ValueSelect and subquery operators, Derived/LATERAL, sets and read/recursive common tables, UNNEST/ordinality/parallel arrays, ARRAY subqueries and source-free SELECT. Own graph validation and query-source metadata adapters. Consume the existing expression visitor and provide final output/key lineage to B and effect attachment slots to C. No edits to driver execution or release manifests.

Acceptance includes zero/many scalar results, SQL NULL truth tables, alias/issuer escapes, set multiplicity, recursive scope/cancellation, materialization/correlation probes and intermediate mapping without fake Views. Produce generic examples and source-inventory adapter hooks. Notify integration before changing frozen scope or result identities.

## B — windows, lock clauses and result proofs

Own window functions/frame grammar and phase legality, multiple explicit row locks, query-boundary key proofs, and list/count behavior over the new result graph. Maintain independent data/count bindings and v0.4.3 byte-level compatibility. Also own Host and the existing shared advisory-lock regression matrix. Consume A's result-source hooks; do not reimplement query composition or write execution.

Acceptance includes all native window families/frames, invalid phase use, ties/NULL/collation, count cardinality across nesting, no seek pushdown through semantic boundaries, per-target lock contention and missing grants. Export required readiness premises. Materialized volatile values must be repeatable within a row but still excluded from inappropriate key/winner proofs.

## C — typed writes

Own InsertInto/UpdateOf/DeleteFrom, target/default/conflict rules, finite batch shapes, mutation RETURNING registration and modifying common-table nodes. Require unique matching evidence for value-bearing FROM/USING, preserve minimal target/RHS grants, and export exact effect graphs. Use A's read graph for INSERT SELECT and CTE results. Consume the integration owner's authorized driver/result-stream APIs rather than introducing a raw fallback.

Acceptance includes minimum-role positive/negative cases, defaults/sequence/identity behavior, version/once/soft-delete guards, generic partial-conflict inference, returned trigger values, write CTE snapshots, duplicate target rejection and batch/iterator failures. Include UNNEST through proven Derived matching in UPDATE FROM. No changes to read Page/List API or expression signature tables without coordination.

## Integration owner

Own protocol verification backend and additive sqlcheck entry points, signed write policy handling, common authorized execution/cleanup semantics, generated docs, CollectSQL final integration, public API/size/neutrality review and all release steps. Own cross-family tests, especially materialized volatile values in write sources, windows over grouped CTEs, nested arrays/JSON bindings, counts over set results, read-only refusal of outer-SELECT writes and non-executing result verification.

Workers supply their own focused unit and disposable PG tests, lint and evidence; integration replays the combined tree with no suppressions on PostgreSQL 14–17 and the two supported Go versions. Heavy jobs share the existing host lock and bounded Go concurrency. Never use another project's databases or cancel another job to advance this work.

## Estimate after design approval

| Stage | Working days | Exit condition |
| --- | --- | --- |
| Shared contract, protocol proof and compile fixtures | 1–2 | Reviewed signatures and query/effect lifecycle; reference cases resolved |
| A/B/C in parallel, integration backend alongside | 5–8 | Package-level dual-driver tests, exact metadata and clean local commits |
| Cross-family integration and complete matrix | 3–4 | --all, no-exemption lint, API/snapshot/privacy checks, candidate PR 12/12 |
| Release preparation and handoff | 1 | Final candidate evidence and audit packet ready |

Total estimate: 10–15 working days to a reviewable release candidate, assuming access to the shared test host. External audit turnaround and extended host queues are not hidden inside that estimate. Do not shorten it by dropping a named family, bypassing a result-description check or shipping a subset under the complete M3 label. If a review changes the verifier dependency or recursive scope, revise this estimate explicitly before implementation.

The added array/query forms remain in A; shared advisory regression and Host remain in B. C consumes the same QuerySource contract. The 10–15 working-day estimate is unchanged; shared test-host queues and final audit remain external to it.
