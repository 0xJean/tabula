# Typed PostgreSQL expressions

Scalar expressions live in `github.com/0xJean/tabula/plan` (`package plan`) and compose in projections, predicates and `Assign` values. Each
node retains its PostgreSQL type, input identity, nullability and stability.
Constructors accept typed handles, never SQL fragments or function names. A
`TryDefine` constructor rejects an invalid tree before registering any shape.

```go
fallback := plan.Input("fallback", pg.Text())
key := plan.Input("key", pg.Text())
label := plan.Coalesce(plan.JSONGetText(document.Expr(), key.Expr()), fallback.Expr())
normalized := plan.Lower(plan.BTrim(label))
assignment := plan.Assign(target, normalized)
```

Here `document` and `target` are fields on the declared occurrence. Bind values
with `BindValue`; repeated uses of a slot share one parameter position. `Assign`
evaluates its values on the old row. Its left side requires UPDATE on that column.
SELECT comes only from actual fields in the value, WHERE, scope, live-row or
version predicate. A slot, clock or generated UUID can overwrite a column without
SELECT on that column. Protected columns, write modes and read-only sessions keep
their existing enforcement.

## Closed operations

| Family | Operations and result |
| --- | --- |
| Conditional | `Coalesce`, `Greatest`, `Least`, `NullIf`, `Case(When(...)).Else(...)`/`ElseNull`, `TypedNull` preserve an exact result catalog type. |
| Arithmetic | `Plus`, `Minus`, `Times`, `Divide`; explicit `...As(result,a,b)` for cross-width integers, date/int4, date or timestamp/interval, timestamp differences and interval/float8 scaling. |
| Text | `TextCat`, `Concat`, `Lower`, `Upper`, `Trim`, `BTrim`, `Substr`, `RegexpReplace` return text; `Length` returns int4. `StartsWith` and the four `Regexp...Match` variants return boolean. Text helpers accept text and native varchar/bpchar coercions. |
| Time | `Now` and `CurrentTimestamp` return timestamptz; `CurrentDate` returns date. `DateTrunc`, `DateTruncAt`, `Timezone`, `Extract` and `ToChar` check native source/result pairs. |
| JSON | `JSONGet`, `JSONGetAt`, `JSONGetPath` preserve json/jsonb and return raw documents. Their text counterparts and `JSONTypeOf` return text. `JSONBuildObject(result, JSONPair(...))` selects the json or jsonb builder and the declared result codec. |
| JSONB | `JSONBCat`, `JSONBDeleteKey/Keys/At/Path`, `JSONHasKey`, `JSONContains`, `JSONContainedBy` require jsonb. Editing returns a raw JSONB document because the shape can change. |
| Array | `ArrayExpr`, `ArrayCat`, `ArrayRemove`, `Cardinality`, `ArrayContains`, `ArrayContainedBy`, `ArrayOverlaps`, `NotEqualAll` retain concrete element identity. `ArrayExpr` requires at least one element; bind a typed empty array when needed. |
| Bytes | `ByteaCat`, `ConvertTo`, `SHA256` return bytea. `Encode` selects `Hex`, `Base64` or `Escape` and returns text. |
| UUID | `GenRandomUUID` returns a UUID represented by a Go string. `GenRandomUUIDAs(pg.UUID[V]())` declares a different result mapping. Both are non-null and volatile. |

`Extract` requires a numeric result, for example `pg.Numeric[string]()`. Units
are closed `DatePart` constants, checked against the source's native supported
fields. `ToChar(date,format)` explicitly converts date to timestamptz, matching
PostgreSQL's native overload resolution. This preserves session-zone fields such
as `TZ` and `OF`; casting date to timestamp first has different formatting.

The text, regex, JSON keys and paths, zones, encoding names and delimiters remain
bound values. Only closed grammar units and encode formats enter SQL as literals.
`ConvertTo` explicitly casts its encoding operand to pg_catalog.name, as required
by `convert_to(text,name)`. Invalid runtime patterns, encodings, conversions,
numeric overflow and division by zero retain their PostgreSQL SQLSTATE.

## Cast matrix

`Cast(result,value)` admits identity conversions of reviewed built-ins, all pairs
among int2/int4/int8/float4/float8/numeric, native character conversions, conversions
between reviewed scalar types and text/varchar/bpchar, json/jsonb in either
direction, and the temporal pairs below. One-dimensional element-array casts
reuse the scalar matrix. Arrays can also convert to/from text through native I/O.

| Source | Non-text temporal destinations |
| --- | --- |
| date | timestamp, timestamptz |
| timestamp | date, time, timestamptz |
| timestamptz | date, time, timetz, timestamp |
| time | interval, timetz |
| timetz | time |
| interval | time |

Destination modifiers are rejected: use `pg.Varchar()` for a cast and retain
`pg.Varchar(n)` for physical column verification. Parameter casts remain
typmod-free. Application-defined enums, domains, casts and operators are outside
these closed conversions; their existing field and input mappings remain usable.

## NULL, stability and exact grants

Strict functions propagate SQL NULL after outer-join expansion. `Coalesce`,
`Greatest` and `Least` are non-null if any operand is guaranteed non-null. CASE
uses the nullability of its result arms, not its conditions. `Concat` skips SQL
NULL and is non-null. `ArrayCat` preserves a non-null side; `ArrayRemove` can remove
NULL elements and returns SQL NULL only for a NULL array.

JSON null differs from SQL NULL. Missing paths and text extraction of JSON null
yield SQL NULL. An object's SQL NULL value becomes JSON null; a SQL NULL key is a
database error. JSON keeps duplicate object members, while JSONB keeps the last
value. Both formats accept up to 50 ordered object pairs.

`NotEqualAll` follows three-valued SQL logic: an empty array yields true, a match
yields false, and a nonmatching array with NULL elements yields NULL. Constructed
arrays preserve NULL elements. Select an output codec that preserves individual
NULLs; an ordinary Go zero value is not a substitute for SQL NULL. To project an
array built from nullable fields, use
`ArrayExprAs(pg.ArrayOfNullable(pg.Int4()), rank.Expr())` and a `[]*int32` array
output. The same mapping preserves NULL elements introduced by a LEFT JOIN.
`ArrayRemove` retains that declared array mapping while accepting a scalar
`Expr[int32]` removal operand, including `TypedNull(pg.Int4())` to remove NULLs.

The compiler records exact EXECUTE requirements for explicit functions, operators
and casts, including native polymorphic signatures and I/O casts. It does not
invent caller grants on functions invoked internally by an aggregate, comparator
or array helper. Schema-qualified operations cannot resolve application overloads
through search_path. Composing an existing aggregate into a new expression or a
joined list records its native signature in each shape that evaluates it.
An exact count can omit aggregates used only to present values. New forms using
MIN or MAX require a known native overload with the declared result type.
Existing declarations retain their registered SQL and grant snapshots.

Native NULLIF resolves an unqualified equality operator. Tabula instead uses
qualified array removal and a subscript for scalar operands, evaluating each
operand once in its original SELECT or UPDATE level. Array operands use a qualified
CASE comparison; the first operand must contain no VOLATILE node. Immutable and
statement-STABLE array first operands and volatile array second operands remain
legal. A volatile array first operand fails at declaration with a diagnostic.

Clocks, formatting, value-to-text concatenation and session-dependent conversions
cannot establish a repeatable page boundary. UUID generation is VOLATILE even
though it is read-only. Intrinsic stability and every child's stability participate
in the shared expression analysis. Secret physical reads propagate through nested
expressions to observations; input values never enter statement keys or snapshots.
