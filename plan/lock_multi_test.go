package plan

import (
	"github.com/0xJean/tabula"

	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadMultipleLocks(t *testing.T) {
	type item struct {
		ID int64 `db:"id,key"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	a := MapTable(tabula.DefinePlanSource(reg, tabula.TableSpec[item, int64]{Name: "public.items", RelationScope: tabula.Global()})).As("i")
	b := MapTable(tabula.DefinePlanSource(reg, tabula.TableSpec[item, int64]{Name: "public.notes", RelationScope: tabula.Global()})).As("n")
	id, other := FieldOf(a.Rel(), "id", Int8()), FieldOf(b.Rel(), "id", Int8())
	out := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "entries"})
	base := Select(Project(Output(out, "id", Int8()), id.Expr())).From(a).InnerJoin(b, Eq(id.Expr(), other.Expr()))
	first, second := ForRows(ForUpdate, a.Rel(), NoWait), ForRows(ForShare, b.Rel(), SkipLocked)
	query := base.Locks(first, second)
	_, err := TryDefineRead(out, "both", query)
	require.NoError(t, err)
	def := out.Definition()
	require.Contains(t, def.Statements["both.global.data"], ` FOR UPDATE OF "i" NOWAIT FOR SHARE OF "n" SKIP LOCKED`)
	require.Equal(t, []string{`"public"."items"`, `"public"."notes"`}, def.StatementShapes["both.global.data"].AnyColumnUpdate)
	require.Equal(t, tabula.EffectRead|tabula.EffectRowLock, def.StatementShapes["both.global.data"].Effects)
	_, err = TryDefineRead(out, "replace", query.Lock(second))
	require.NoError(t, err)
	require.NotContains(t, out.Definition().Statements["replace.global.data"], "FOR UPDATE")
	_, err = TryDefineRead(out, "clear", query.Lock(RowLock{}))
	require.NoError(t, err)
	require.NotContains(t, out.Definition().Statements["clear.global.data"], "FOR SHARE")
	args := []RowLock{second}
	immutable := base.Locks(first, args...)
	args[0] = first
	_, err = TryDefineRead(out, "copied", immutable)
	require.NoError(t, err)
	before := out.Definition()
	for _, bad := range []ReadSelect{
		base.Locks(first, first), base.Locks(RowLock{}), query.WithList(ReadListSpec{ExactCount: true}),
		query.GroupBy(id.Expr()), query.Distinct(),
		Select(Project(Output(out, "id", Int8()), Over(RowNumber(), WindowSpec{}))).From(a).Locks(first),
		Select(Project(Output(out, "id", Int8()), id.Expr())).From(a).LeftJoin(b, Eq(id.Expr(), other.Expr())).Locks(first, second),
		Select(Project(Output(out, "id", Int8()), id.Expr())).From(a).Locks(second),
	} {
		_, err := TryDefineRead(out, "bad", bad)
		require.Error(t, err)
		require.Equal(t, before, out.Definition())
	}
	_, err = TryDefineRead(out, "paged", query.WithList(ReadListSpec{Orders: []RowOrder{{Name: "id", Terms: []RowTerm{Asc(id)}}}, Pages: RowPages{Offset: true, MaxOffset: 10, MaxLimit: 5}}))
	require.NoError(t, err)
	require.Contains(t, out.Definition().Statements["paged.global.list.data.sort.id.offset"], ` LIMIT CAST($1 AS "pg_catalog"."int8") OFFSET CAST($2 AS "pg_catalog"."int8") FOR UPDATE OF "i" NOWAIT FOR SHARE OF "n" SKIP LOCKED`)
}

func TestHostIdentityAndNullability(t *testing.T) {
	inet := tabula.CatalogType[string]("pg_catalog", "inet", nil)
	value := Input("address", inet)
	query, c := renderScalar(t, Host(value.Expr()))
	require.Equal(t, `pg_catalog."host"(CAST($1 AS "pg_catalog"."inet"))`, query)
	require.Contains(t, c.functions, "pg_catalog.host(inet)")
	require.True(t, repeatableExpression(Host(value.Expr()).part))
	require.False(t, Host(value.Expr()).nullable)
	require.True(t, Host(TypedNull(inet)).nullable)
	for _, wrong := range []Expr[string]{Input("text", Text()).Expr(), Input("cidr", tabula.CatalogType[string]("pg_catalog", "cidr", nil)).Expr(), Input("domain", tabula.CatalogType[string]("demo", "address", nil)).Expr(), {}} {
		_, err := Host(wrong).part.render(c)
		require.Error(t, err)
	}
}
