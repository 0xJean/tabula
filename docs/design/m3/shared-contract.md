# Shared implementation contract

The approved design is implemented in three independent packages from one frozen integration commit. The types in this document are internal seams, not a public SQL escape. Existing physical-only execution stays unchanged until a complete new graph is compiled and registered.

## Query and statement seams

- `statementNode.statementSources() ([]rowSource, error)` walks all physical leaves in deterministic declaration order, including unreferenced modifying common tables. Walk before allocating the final shared parameter layout. Deduplicate a repeated declaration by identity, not by its printed alias.
- `statementNode.renderStatement(*statementContext) (statementResult, error)` renders one block with an inherited parameter layout. `statementContext` carries the shared typedCompiler, lexical queryBlock, statementPosition and optional final output registration/indexes. Intermediate results need no decoder or fabricated View.
- `statementResult` carries SQL, ordered queryResultField values, queryResultKey premises, StatementShape and statementEffects. `frozen()` copies all mutable exported slices and preserves identity tokens. The renderer never consults runtime values.
- `queryBlock` checks physical/result occurrences by identity at exactly one lexical depth. Local aliases must be distinct. Child blocks may reuse aliases; Outer must not infer visibility by spelling. Join visibility is accumulated from left to right. Common-table bindings and recursive legality are added by A using the same identity boundary.
- `queryResultField` retains a nonzero-sized queryFieldIdentity, name, scalarOperand, secrecy, physical origins and original stability. `stored` means a materialized result slot is repeatable within its row. It does not replace stability or prove traversal consistency.
- `queryResultKey` contains exported field identities plus actual catalog KeyRequirement/ComparisonRequirement premises. Neither a source name, UNNEST ordinal nor window rank creates a key automatically.
- `statementEffects.combine` records all physical write commands and merges reused common-table effects once. Independent writes to one physical target fail atomically. C must retain the same effect identity for repeated references to one CTE. A single conflict-update node retains one identity with both INSERT and UPDATE commands; deduplicate by identity, target and command. Its returning flag belongs to that physical write, not to an unrelated outer result. The integration owner maps this immutable record to signed policy checks and portable snapshot requirements.
- Public WriteStatement and ResultStatement are sealed wrappers around statementNode. C supplies write nodes; A supplies read graphs and the top-level SELECT containing modifying common tables. A returning registration still checks for a real write effect.

A owns new public QuerySource, Exported, ValueSelect and CommonTable constructors. Use the approved design signatures. ReadSelect remains its existing concrete type. Preserve the old compiler path byte-for-byte for unchanged declarations; new graphs use lexical context and the same binding/authorization machinery. A supplies an early compile checkpoint for B/C. B and C must not create temporary public stubs or alternate query compilers.

## File ownership and integration

| Owner | Files and responsibilities | Shared edits requiring coordination |
| --- | --- | --- |
| A | New query/source/export/subquery/set/CTE files; typed_join.go and typed_join_compile.go query routing; source collection; source-free SELECT | expression field resolution, Projection intermediate identities, expression visitor query boundaries; notify B/C before changing the seam |
| B | New window/frame/lock/Host files; existing grouping/order/list-proof integration; shared advisory tests | Leave A query routing alone; provide render/phase/proof helpers and small integration patch where needed |
| C | New write/conflict/default/batch/returning/write-CTE files; mutation integration | Consume A sources and root operation hook; do not add raw execution or change ReadSelect's source model |
| Integration | sqlcheck verifier and pgx adapter; OperationSession implementations; signed policy; final metadata/CollectSQL integration and release | Coordinate shared descriptor/compiler changes; own root policy and drivers |

No worker edits release manifests, generated REFERENCE, global CHANGELOG, README or this shared contract until integration; each updates owned GoDoc and a focused generic feature guide. Supply the proposed global documentation changes in the handoff. Keep production files at most 500 lines and tests at most 600. Preserve existing public signatures and mark positional StatementPolicy literal considerations in the final migration guide.

## Driver and verification seams

OperationSession.WithinOperation receives a sealed child and child context; root owns both implementations. Write callers prevalidate bindings, then issue all calls against that child. No raw SAVEPOINT fallback. Missing capability returns ErrOperationRequired before I/O. Result decoding, iteration and batch cleanup must respect the approved all-or-error contract.

sqlcheck.Verifier/VerificationTx/Description/DescribedField are the frozen standard-library interface in sqlcheck/verifier.go. Root owns the read-only transaction algorithm and pgxdriver.SQLVerifier. Use ErrDescribeRequired when a backend cannot safely describe a write result. C must provide exact logical result lineage and conflict-inference metadata; do not execute a result probe or consider a declared type to be server evidence.

## Verification of the shared freeze

The initial shared tests exercise identity lookup, explicit outer depth, duplicate aliases, foreign registries, immutable result/key metadata, physical write-effect conflicts and reused effect identities. They do not claim to validate a new query renderer or a driver. Full feature tests and the cross-family matrix remain mandatory before the candidate PR.

## Portable write metadata

StatementShape carries optional UniqueMatches, Defaults, Conflicts, ReturningRelations and ResultLineage. All slices and nested column lists are copied when exported. UniqueMatchRequirement records an immediate non-null unique source premise independently of the existing primary-key KeyRequirement. DefaultRequirement records the physical columns whose database values are used. ConflictRequirement records the target relation, a constraint or inference columns, and whether the conflict arm updates; the original SQL and actual EXPLAIN arbiters supply the predicate evidence. ReturningRelations records only actual physical RETURNING targets. A result from another source does not widen an effect-only insert's permissions.

ResultLineage follows Result positions. A direct Relation/Column needs independent RowDescription origin and catalog agreement. A CastType needs a matching explicit outer result conversion in the original SQL, not merely a declared type string. Native results may use empty entries. Unknown logical domain lineage fails clearly; a matching flattened base OID cannot establish a domain. Actual read-only queries can retain the existing logical-type probe. The integration owner implements these backend checks before a candidate can claim them as verified.
