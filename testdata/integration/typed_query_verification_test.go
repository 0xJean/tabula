//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
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
		id := q.FieldOf(f.src.Rel(), "id", q.Int8())
		value := q.Input("value", q.Int8())
		key, extra := q.Export("id", id.Expr()), q.Export("value", value.Expr())
		derived := q.Derived("d", q.Select(key.Project(), extra.Project()).From(f.src))
		plan := q.DefineRead(out, "listed", q.Select(
			q.Project(q.Output(out, "id", q.Int8()), key.From(derived)),
			q.Project(q.Output(out, "value", q.Int8()), extra.From(derived)),
		).From(derived).WithList(q.ReadListSpec{
			Orders:     []q.RowOrder{{Name: "id", Terms: []q.RowTerm{q.AscExpr(key.From(derived))}}},
			Pages:      q.RowPages{Offset: true, MaxLimit: 2, MaxOffset: 4},
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
			rows, count, err := plan.Bind(s).List(ctx, q.RowChoice{Order: "id", Page: q.OffsetPage, Limit: 2}, q.BindValue(value, int64(7)))
			require.NoError(t, err)
			require.Equal(t, []row{{ID: 1, Value: 7}}, rows)
			require.EqualValues(t, 1, count)
			return nil
		}))
	})
}
