package tabula

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNativeHashAndDateTime(t *testing.T) {
	s := Input("value", Text())
	seed := Input("seed", Int8())
	text, c := renderScalar(t, HashTextExtended(s.Expr(), seed.Expr()))
	require.Contains(t, text, `pg_catalog."hashtextextended"(`)
	require.Contains(t, c.functions, "pg_catalog.hashtextextended(text,int8)")
	require.True(t, repeatableExpression(HashTextExtended(s.Expr(), seed.Expr()).part))
	require.True(t, HashTextExtended(TypedNull(Text()), seed.Expr()).nullable)
	require.True(t, HashTextExtended(s.Expr(), TypedNull(Int8())).nullable)
	_, err := HashTextExtended(s.Expr(), Input("small", Int4()).Expr()).part.render(c)
	require.Error(t, err)
	_, err = HashTextExtended(Input("domain", CatalogType[string]("demo", "label", nil)).Expr(), seed.Expr()).part.render(c)
	require.Error(t, err)
	day := Input("day", CatalogType[time.Time]("pg_catalog", "date", nil)).Expr()
	clock := Input("clock", CatalogType[string]("pg_catalog", "time", nil)).Expr()
	stamp := CatalogType[time.Time]("pg_catalog", "timestamp", nil)
	for _, value := range []Expr[time.Time]{PlusAs(stamp, day, clock), PlusAs(stamp, clock, day)} {
		_, c := renderScalar(t, value)
		require.True(t, c.functions["pg_catalog.datetime_pl(date,time)"] || c.functions["pg_catalog.timedate_pl(time,date)"])
		require.True(t, repeatableExpression(value.part))
	}
	_, err = PlusAs(Timestamptz(), day, clock).part.render(c)
	require.Error(t, err)
	require.True(t, PlusAs(stamp, TypedNull(day.kind), clock).nullable)
}
