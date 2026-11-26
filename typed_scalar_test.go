package tabula

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func renderScalar(t *testing.T, value Scalar) (string, *typedCompiler) {
	t.Helper()
	c, err := newTypedCompiler(false)
	require.NoError(t, err)
	v := value.scalarOperand()
	query, err := v.part.render(c)
	require.NoError(t, err)
	require.NoError(t, checkRowExpr(v.part, nil))
	_, ok := expressionStructure(v.part)
	require.True(t, ok)
	require.True(t, sameExpression(v.part, v.part))
	return query, c
}

func TestScalarFamilies(t *testing.T) {
	s := Input("text", Text()).Expr()
	i := Input("integer", Int4()).Expr()
	big := Input("big", Int8()).Expr()
	b := Input("flag", Bool()).Expr()
	bytes := Input("bytes", CatalogType[[]byte]("pg_catalog", "bytea", nil)).Expr()
	arr := Input("array", ArrayOf(Int4())).Expr()
	path := Input("path", ArrayOf(Text())).Expr()
	doc := Input("doc", CatalogType[json.RawMessage]("pg_catalog", "jsonb", nil)).Expr()
	stamp := Input("stamp", Timestamptz()).Expr()
	day := Input("day", CatalogType[time.Time]("pg_catalog", "date", nil)).Expr()
	span := Input("span", CatalogType[string]("pg_catalog", "interval", nil)).Expr()
	numeric := CatalogType[string]("pg_catalog", "numeric", nil)
	cases := []Scalar{
		Coalesce(TypedNull(Text()), s), Greatest(s, s), Least(i, i), NullIf(s, s),
		Case(When(b, s)).Else(s), Case(When(b, s)).ElseNull(),
		Plus(i, i), Minus(i, i), Times(i, i), Divide(i, i), PlusAs(Int8(), i, big),
		MinusAs(CatalogType[string]("pg_catalog", "interval", nil), stamp, stamp),
		TimesAs(numeric, Cast(numeric, i), Cast(numeric, big)), DivideAs(Int8(), big, i),
		TextCat(s, s), ByteaCat(bytes, bytes), Concat(s, i, doc), Lower(s), Upper(s), Trim(s), BTrim(s, s), Length(s),
		Substr(s, i), Substr(s, i, i), StartsWith(s, s), RegexpReplace(s, s, s), RegexpReplace(s, s, s, s),
		RegexpMatch(s, s), RegexpIMatch(s, s), RegexpNotMatch(s, s), RegexpNotIMatch(s, s),
		Now(), CurrentTimestamp(), CurrentDate(), DateTrunc(Day, stamp), DateTrunc(Hour, span), DateTruncAt(Day, stamp, s),
		Timezone(s, stamp), Timezone(s, Cast(CatalogType[time.Time]("pg_catalog", "timestamp", nil), stamp)),
		Extract(numeric, Epoch, stamp), ToChar(stamp, s), ToChar(day, s),
		JSONGet(doc, s), JSONGetAt(doc, i), JSONGetText(doc, s), JSONGetTextAt(doc, i), JSONGetPath(doc, path), JSONGetTextPath(doc, path),
		JSONHasKey(doc, s), JSONContains(doc, doc), JSONContainedBy(doc, doc), JSONTypeOf(doc), JSONBCat(doc, doc),
		JSONBDeleteKey(doc, s), JSONBDeleteKeys(doc, path), JSONBDeleteAt(doc, i), JSONBDeletePath(doc, path),
		JSONBuildObject(doc.kind), JSONBuildObject(doc.kind, JSONPair(s, i)), JSONBuildObject(CatalogType[json.RawMessage]("pg_catalog", "json", nil), JSONPair(s, i)),
		ArrayExpr(i, TypedNull(Int4())), ArrayCat(arr, arr), ArrayContains(arr, arr), ArrayContainedBy(arr, arr), ArrayOverlaps(arr, arr),
		NotEqualAll(i, arr), Cardinality(arr), ArrayRemove(arr, i),
		ArrayExprAs(ArrayOfNullable(Int4()), i, TypedNull(Int4())),
		ConvertTo(s, s), Encode(bytes, Hex), Encode(bytes, Base64), Encode(bytes, Escape), SHA256(bytes), GenRandomUUID(),
	}
	for _, value := range cases {
		query, c := renderScalar(t, value)
		require.NotEmpty(t, query)
		for fn := range c.functions {
			require.NotEmpty(t, fn)
		}
	}
	query, c := renderScalar(t, Plus(i, i))
	require.Equal(t, `(CAST($1 AS "pg_catalog"."int4") OPERATOR(pg_catalog.+) CAST($1 AS "pg_catalog"."int4"))`, query)
	require.Len(t, c.params.order, 1)
	require.Equal(t, map[string]bool{"pg_catalog.int4pl(int4,int4)": true}, c.functions)
	query, c = renderScalar(t, ToChar(day, s))
	require.Contains(t, query, `AS "pg_catalog"."timestamptz"`)
	require.Contains(t, c.functions, "pg_catalog.to_char(timestamptz,text)")
	require.Contains(t, c.functions, "pg_catalog.timestamptz(date)")
}

func TestScalarNullabilityAndIdentity(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	rel, _ := scopedRowSource(reg, TableSpec[rowPlanItem, int64]{Name: "public.items"})
	rank := FieldOf(rel, "rank", Int4()).Expr()
	i := Input("fallback", Int4()).Expr()
	outer := map[*occurrence]bool{rel.source: true}
	for _, v := range []Scalar{Plus(rank, i), NullIf(rank, i), Case(When(IsNull(rank), rank)).Else(i), ArrayRemove(ArrayExpr(i), rank)} {
		want := true
		if v.scalarOperand().kind.element != nil {
			want = false
		}
		require.Equal(t, want, v.scalarOperand().nullableWith(outer))
	}
	for _, v := range []Scalar{Coalesce(rank, i), Greatest(rank, i), Least(rank, i), Concat(rank, i), ArrayExpr(rank), Case(When(Eq(rank, i), i)).Else(i)} {
		require.False(t, v.scalarOperand().nullableWith(outer))
	}
	require.True(t, repeatableExpression(Plus(rank, i).part))
	for _, v := range []Scalar{Now(), CurrentTimestamp(), CurrentDate(), GenRandomUUID(), Concat(i), Cast(Text(), Now())} {
		require.False(t, repeatableExpression(v.scalarOperand().part))
	}
	require.True(t, sameExpression(Plus(rank, i).part, Plus(rank, i).part))
	require.False(t, sameExpression(Plus(rank, i).part, Minus(rank, i).part))
	require.False(t, sameExpression(Plus(rank, i).part, Plus(rank, Input("fallback", Int4()).Expr()).part))
	require.False(t, sameExpression(Encode(TypedNull(CatalogType[[]byte]("pg_catalog", "bytea", nil)), Hex).part,
		Encode(TypedNull(CatalogType[[]byte]("pg_catalog", "bytea", nil)), Base64).part))
}

func TestScalarInvalidDeclarations(t *testing.T) {
	s := Input("s", Text()).Expr()
	i := Input("i", Int4()).Expr()
	custom := Input("custom", CatalogType[string]("demo", "label", nil)).Expr()
	jsonDoc := TypedNull(CatalogType[json.RawMessage]("pg_catalog", "json", nil))
	pairs := slices.Repeat([]JSONMember{JSONPair(s, i)}, 51)
	cases := []Scalar{
		Cast(Varchar(2), s), Cast(Text(), custom), Cast(Bool(), i), Plus(s, s), PlusAs(Int4(), i, Input("wide", Int8()).Expr()),
		Lower(custom), Coalesce(s, custom), NullIf(custom, custom), Greatest(jsonDoc, jsonDoc),
		Case(When(Pred{}, s)).Else(s), Case(When(IsNull(i), s)).Else(custom), CaseExpr[string]{}.Else(s),
		BTrim(s, s, s), Substr(s, i, i, i), RegexpReplace(s, s, s, s, s),
		Concat(nil), Concat(custom), ArrayExpr(Expr[int32]{}), ArrayCat(Input("bytes", CatalogType[[]byte]("pg_catalog", "bytea", nil)).Expr(), TypedNull(CatalogType[[]byte]("pg_catalog", "bytea", nil))),
		ArrayExprAs(ArrayOfNullable(Text()), i), ArrayExprAs(Text(), s),
		JSONHasKey(jsonDoc, s), JSONBCat(jsonDoc, jsonDoc), JSONGet(Expr[string]{}, s), JSONBuildObject(Text()), JSONBuildObject(jsonDoc.kind, pairs...),
		Cardinality(s), ArrayRemove(Input("a", ArrayOf(Int4())).Expr(), s), ArrayContains(s, s),
		NotEqualAll(i, Input("wrong_array", ArrayOf(Text())).Expr()), NotEqualAll(i, s),
		GenRandomUUIDAs(Text()), DateTrunc(Day, s), DateTrunc(DatePart(255), Now()), DateTruncAt(Day, CurrentDate(), s),
		Extract(Text(), Epoch, Now()), Extract(CatalogType[string]("pg_catalog", "numeric", nil), Hour, CurrentDate()), Timezone(s, s), ToChar(s, s),
		Encode(TypedNull(CatalogType[[]byte]("pg_catalog", "bytea", nil)), EncodeFormat(0)),
		TypedNull(PGType[string]{}), Coalesce(Expr[string]{}, s),
		NullIf(ArrayExpr(GenRandomUUID()), TypedNull(ArrayOf(CatalogType[string]("pg_catalog", "uuid", nil)))),
	}
	for _, value := range cases {
		c, err := newTypedCompiler(false)
		require.NoError(t, err)
		_, err = value.scalarOperand().part.render(c)
		require.Error(t, err, "%#v", value)
	}
}

func TestScalarNullIfArrayStability(t *testing.T) {
	s := Input("value", Text()).Expr()
	for _, v := range []Scalar{
		NullIf(ArrayExpr(s), ArrayExpr(Cast(Text(), GenRandomUUID()))),
		NullIf(ArrayExpr(Concat(s)), ArrayExpr(Cast(Text(), GenRandomUUID()))),
	} {
		query, c := renderScalar(t, v)
		require.Contains(t, query, "OPERATOR(pg_catalog.=)")
		require.Contains(t, c.functions, "pg_catalog.array_eq(anyarray,anyarray)")
	}
}

func TestScalarJSONResultBinding(t *testing.T) {
	doc := Input("doc", scalarJSONKind("jsonb")).Expr()
	key := Input("key", Text()).Expr()
	for _, expr := range []Expr[json.RawMessage]{JSONGet(doc, key), JSONBCat(doc, doc), JSONBDeleteKey(doc, key)} {
		raw := json.RawMessage(`{"x":1}`)
		bound := expr.kind.id.bind(nil, raw)
		require.NoError(t, bound.err)
		raw[5] = '2'
		require.Equal(t, `{"x":1}`, bound.value)
		require.Error(t, expr.kind.id.bind(nil, json.RawMessage(`{`)).err)
		require.Nil(t, expr.kind.id.bind(nil, json.RawMessage(nil)).value)
	}
}

func TestScalarNullIfVolatileArrayDeclaration(t *testing.T) {
	type item struct {
		ID int64 `db:"id,key"`
	}
	type result struct {
		Value *string `db:"value"`
	}
	reg := NewRegistry(RegistryConfig[struct{}]{})
	table := DefinePlanSource(reg, TableSpec[item, int64]{Name: "public.items", RelationScope: Global()})
	src := MapTable(table).As("i")
	view := DefineView(reg, ViewSpec[result]{Name: "entries"})
	value := Cast(Text(), NullIf(ArrayExpr(GenRandomUUID()), ArrayExpr(GenRandomUUID())))
	query := Select(Project(Output(view, "value", Text()), value)).From(src)
	var diagnostic string
	for range 2 {
		_, err := TryDefineRead(view, "invalid", query)
		require.ErrorContains(t, err, "array first operand without VOLATILE expressions")
		if diagnostic == "" {
			diagnostic = err.Error()
		}
		require.EqualError(t, err, diagnostic)
		require.Empty(t, view.Definition().Statements)
		require.Empty(t, view.Definition().StatementShapes)
		require.Empty(t, table.Definition().Statements)
	}
}
