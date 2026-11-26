package tabula

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type joinItem struct {
	ID    int64  `db:"id,key"`
	Scope int32  `db:"scope_id,scope"`
	Name  string `db:"name"`
}

func TestExplicitJoinScopeAndNullability(t *testing.T) {
	reg := NewRegistry(RegistryConfig[int32]{})
	table := Define(reg, TableSpec[joinItem, int64]{Name: "public.items"})
	a, b := FromTable(table, "a", ByScopeColumn()), FromTable(table, "b", ByScopeColumn())
	id, other := FieldOf(a, "id", Int8()), FieldOf(b, "id", Int8())
	type pair struct {
		ID    int64  `db:"id"`
		Other *int64 `db:"other"`
	}
	out := DefineView(reg, ViewSpec[pair]{Name: "entries"})
	key := Input("key", Int8())
	base := Select(Project(Output(out, "id", Int8()), id.Expr()), Project(Output(out, "other", Int8()), other.Expr())).From(a)
	query := base.LeftJoin(b, Eq(id.Expr(), other.Expr())).Where(Ge(id.Expr(), key.Expr())).OrderBy(Asc(id))
	_, err := TryDefineRead(out, "pairs", query.AllowUnscoped())
	require.NoError(t, err)
	def := out.Definition()
	require.Len(t, def.Statements, 2)
	const shape = "pairs.scoped.data"
	require.Equal(t, `SELECT "a"."id" AS "id", "b"."id" AS "other" FROM "public"."items" AS "a" LEFT JOIN "public"."items" AS "b" ON ("a"."id" = "b"."id") AND ("b"."scope_id" = CAST($1 AS "pg_catalog"."int4")) WHERE ("a"."scope_id" = CAST($1 AS "pg_catalog"."int4")) AND ("a"."id" >= CAST($2 AS "pg_catalog"."int8")) ORDER BY "a"."id" ASC NULLS LAST`, def.Statements[shape])
	s := def.StatementShapes[shape]
	require.Equal(t, []ShapeField{{Name: "id", Type: Int8().id.qualified()}, {Name: "other", Type: Int8().id.qualified(), Nullable: true}}, s.Result)
	require.Equal(t, []ShapeInput{{Name: "scope", Type: Int4().id.qualified(), Scope: true}, {Name: "key", Type: Int8().id.qualified()}}, s.Inputs)
	require.Equal(t, []ShapeAccess{{`"public"."items"`, "SELECT"}}, s.Access)
	require.True(t, def.StatementShapes["pairs.unscoped.data"].UnscopedOnly)
	require.NotContains(t, def.Statements["pairs.unscoped.data"], "scope_id")

	c := FromTable(table, "c", ByScopeColumn())
	later := FieldOf(c, "id", Int8())
	for _, bad := range []ReadSelect{
		base.InnerJoin(b, Eq(later.Expr(), other.Expr())).CrossJoin(c),
		base.From((*Rel[joinItem])(nil)), base.CrossJoin((*Rel[joinItem])(nil)), query.GroupBy((*RowColumn)(nil)),
		base.CrossJoin(a), base.InnerJoin(b, Pred{}), base.InnerJoin(b, Eq(CountAll(), other.Expr())),
		base.LeftJoin(b, Eq(id.Expr(), other.Expr())).Lock(ForRows(ForUpdate, b, Wait)),
		query.GroupBy(id.Expr()).Having(Ge(CountAll(), key.Expr())),
		query.GroupBy(key.Expr()), query.GroupBy(id.Column(), id.Expr()),
		query.GroupBy(id.Column(), other.Column()).Having(Gt(FieldOf(b, "scope_id", Int4()).Expr(), Input("min_scope", Int4()).Expr())),
	} {
		_, err := TryDefineRead(out, "invalid", bad)
		require.Error(t, err)
		require.Equal(t, def, out.Definition())
	}
	strict := DefineView(reg, ViewSpec[typedResult]{Name: "notes"})
	selectRight := Select(Project(Output(strict, "id", Int8()), other.Expr())).From(a)
	_, err = TryDefineRead(strict, "invalid", selectRight.LeftJoin(b, Eq(id.Expr(), other.Expr())))
	require.ErrorContains(t, err, "nullable output")
	_, err = TryDefineRead(strict, "inner", selectRight.InnerJoin(b, Eq(id.Expr(), other.Expr())))
	require.NoError(t, err)
	_, err = TryDefineRead(strict, "count", Select(Project(Output(strict, "id", Int8()), CountOf(other.Expr()))).
		From(a).LeftJoin(b, Eq(id.Expr(), other.Expr())).GroupBy(id.Column()).Having(Ge(CountOf(other.Expr()), key.Expr())).OrderBy(Desc(id)))
	require.NoError(t, err)
	require.Contains(t, strict.Definition().Statements["count.scoped.data"], `GROUP BY "a"."id" HAVING (pg_catalog.count("b"."id") >= CAST($2 AS "pg_catalog"."int8")) ORDER BY "a"."id" DESC`)
}

func TestTypedMembershipAndPatterns(t *testing.T) {
	reg := NewRegistry(RegistryConfig[int32]{})
	table := Define(reg, TableSpec[joinItem, int64]{Name: "public.items", RelationScope: ByRLS()})
	r := FromTable(table, "i", ByRLS())
	id, label := FieldOf(r, "id", Int8()), FieldOf(r, "name", Varchar(20))
	out := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	project := Project(Output(out, "id", Int8()), id.Expr())
	first, second := Input("first", Int8()), Input("second", Int8())
	ids, pattern := Input("ids", ArrayOf(Int8())), Input("pattern", Text())
	where := And(In(id.Expr(), first.Expr(), second.Expr(), first.Expr()), Any(id.Expr(), ids.Expr()),
		Or(Like(label.Expr(), pattern.Expr()), ILike(label.Expr(), pattern.Expr())))
	DefineRows(out, "match", RowsFrom(table, r), RowsSpec{RowFilter: RowFilter{Where: where}}, project)
	def := out.Definition()
	sql := def.Statements["match.rls.data"]
	require.Contains(t, sql, `IN (CAST($1 AS "pg_catalog"."int8"), CAST($2 AS "pg_catalog"."int8"), CAST($1 AS "pg_catalog"."int8"))`)
	require.Contains(t, sql, `= ANY(CAST($3 AS "pg_catalog"."int8"[]))`)
	require.Contains(t, sql, `LIKE CAST($4 AS "pg_catalog"."text")`)
	require.Contains(t, sql, `ILIKE CAST($4 AS "pg_catalog"."text")`)
	require.Len(t, def.StatementShapes["match.rls.data"].Inputs, 4)
	foreign := CatalogType[int64]("public", "item_id", nil)
	wrong := Input("other", ArrayOf(foreign))
	for _, bad := range []Pred{
		In(id.Expr(), Input("other", foreign).Expr()), Any(id.Expr(), wrong.Expr()),
		Any(id.Expr(), Input("bytes", CatalogType[[]int64]("pg_catalog", "bytea", nil)).Expr()),
		ILike(Input("uuid", CatalogType[string]("pg_catalog", "uuid", nil)).Expr(), pattern.Expr()),
		In(id.Expr(), CountAll()),
	} {
		_, err := TryDefineRows(out, "bad", RowsFrom(table, r), RowsSpec{RowFilter: RowFilter{Where: bad}}, project)
		require.Error(t, err)
		require.Equal(t, def, out.Definition())
	}
	require.Panics(t, func() { ArrayOf(PGType[int64]{}) })
	require.Panics(t, func() { ArrayOf(ArrayOf(Int8())) })
}

func TestJoinBindsPrivateValuesPerOccurrence(t *testing.T) {
	type record struct {
		ID    int64  `db:"id,key"`
		State string `db:"state,deleted"`
	}
	reg := NewRegistry(RegistryConfig[customTenant]{})
	source := func(name, alias, live string) Rel[record] {
		table := Define(reg, TableSpec[record, int64]{Name: name, RelationScope: Global(),
			SoftDelete: &SoftDelete{DeletedValue: "closed", AliveValue: live}})
		return FromTable(table, alias, Global())
	}
	a, b := source("public.items", "i", "open"), source("public.groups", "g", "visible")
	out := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	id := FieldOf(a, "id", Int8())
	q := Select(Project(Output(out, "id", Int8()), id.Expr())).From(a).
		LeftJoin(b, Eq(id.Expr(), FieldOf(b, "id", Int8()).Expr()))
	plan := DefineRead(out, "visible_rows", q)
	def := out.Definition()
	const key = "visible_rows.global.data"
	require.NotContains(t, def.Statements[key], "'visible'")
	require.NotContains(t, def.Statements[key], "'open'")
	require.Equal(t, []ShapeInput{
		{Name: "@live.1", Type: Text().id.qualified(), Secret: true},
		{Name: "@live", Type: Text().id.qualified(), Secret: true},
	}, def.StatementShapes[key].Inputs)
	s := &routineSession{authority: reg.SQLAuthority(), method: CallEach}
	_, err := plan.Bind(s).Find(t.Context())
	require.NoError(t, err)
	require.Equal(t, []any{"visible", "open"}, s.lastArgs)
	_, err = plan.Bind(s).Find(t.Context(), BindValue(Input("@live.1", Text()), "other"))
	require.Error(t, err)
	require.Equal(t, 1, s.calls)
}
