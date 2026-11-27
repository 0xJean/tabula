//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

func TestQueryGraphCountVerification(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		t := f.t
		type row struct {
			ID    int64 `db:"id"`
			Value int64 `db:"value"`
		}
		out := q.DefineView(f.reg, q.ViewSpec[row]{Name: "notes"})
		id := tplan.FieldOf(f.src.Rel(), "id", tplan.Int8())
		value := tplan.Input("value", tplan.Int8())
		key, extra := tplan.Export("id", id.Expr()), tplan.Export("value", value.Expr())
		derived := tplan.Derived("d", tplan.Select(key.Project(), extra.Project()).From(f.src))
		plan := tplan.DefineRead(out, "listed", tplan.Select(
			tplan.Project(tplan.Output(out, "id", tplan.Int8()), key.From(derived)),
			tplan.Project(tplan.Output(out, "value", tplan.Int8()), extra.From(derived)),
		).From(derived).WithList(tplan.ReadListSpec{
			Orders:     []tplan.RowOrder{{Name: "id", Terms: []tplan.RowTerm{tplan.AscExpr(key.From(derived))}}},
			Pages:      tplan.RowPages{Offset: true, MaxLimit: 2, MaxOffset: 4},
			ExactCount: true,
		}))
		def := out.Definition()
		require.Contains(t, def.StatementShapes, "listed.global.list.count")
		defs := []q.Definition{q.Definition(def)}
		findings, err := sqlcheck.VerifyStatementsUsing(t.Context(), pgxdriver.SQLVerifier(f.pool), defs, "")
		require.NoError(t, err)
		require.Empty(t, findings)
		findings, err = sqlcheck.VerifyViewStatements(t.Context(), f.db, []q.ViewDefinition{def}, "")
		require.NoError(t, err)
		require.Empty(t, findings)
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			rows, count, err := plan.Bind(s).List(ctx, tplan.RowChoice{Order: "id", Page: tplan.OffsetPage, Limit: 2}, tplan.BindValue(value, int64(7)))
			require.NoError(t, err)
			require.Equal(t, []row{{ID: 1, Value: 7}}, rows)
			require.EqualValues(t, 1, count)
			return nil
		}))
	})
}
