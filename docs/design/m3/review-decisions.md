# M3 approved decisions

The following four API and safety boundaries are approved. Implementation and final candidate verification must preserve them.

1. **Non-executing result verification.** Add a standard-library-only sqlcheck backend contract and a pgxdriver protocol adapter. Existing database/sql verification keeps its signatures but fails clearly on a new write-result shape without Describe capability. A stdsql/lib/pq runtime can use the native adapter for verification only. This avoids executing DML under LIMIT 0 or pretending a generated surrogate proves the exact prepared statement's result type. It adds an optional verification dependency outside core. Domain results require separate logical lineage because the wire protocol reports their base type. The exact proposed backend and operation-savepoint interfaces are in api-contracts.md.
2. **Schema values in partial conflict predicates.** Add a separate closed IndexPredicate API with declaration-time typed schema values, catalog-verified and escaped only in the inference grammar. Ordinary predicates remain fully bound and no literal Expr/Raw is exposed. A forced generic plan cannot generally infer a partial index from a runtime predicate bind. This exception stays confined to index inference.
3. **Single evaluation through an explicit result slot.** Support a volatile array through a materialized CTE or truly correlated materialized lateral source, then apply NullIf to its output field. Keep direct volatile-array lhs rejection where no such boundary exists. Keep volatile provenance for page-key proofs; do not add hidden target reads or a scalar initplan. This completes the explicit relational route without inventing a global once-per-row promise for every syntactic context.
4. **Recursive core scope.** Include read/recursive CTEs, UNION/UNION ALL, explicit termination/cycle predicates and cancellation. Do not expose SQL SEARCH/CYCLE decorators or native record/record-array traversal paths in this release.

Other proposed additions follow the established conventions: Define/TryDefine, Bind(session), explicit operands, exact role grants, immutable canonical bindings, no new core dependency and no new raw SQL path. New opt-in policy flags for signed write CTE/routine results preserve the default guard and are scoped to verified authorized calls.

The shared implementation contracts are frozen independently of feature completion. New public signature disagreements require a focused review; routine implementation does not reopen these decisions.

All four decisions are approved. The array sources, array subquery, source-free SELECT and existing-family additions in ../../DESIGN-M3.md are included in the shared contract. Final candidate audit still precedes merge and release.
