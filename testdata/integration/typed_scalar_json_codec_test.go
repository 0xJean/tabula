//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/plan"
	"github.com/stretchr/testify/require"
)

func TestScalarJSONResultCodecs(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		type object struct {
			Value map[string]int `db:"value,json"`
		}
		type raw struct {
			Value *json.RawMessage `db:"value,json"`
		}
		key, number := plan.Input("key", pg.Text()), plan.Input("number", pg.Int4())
		view := q.DefineView(f.reg, q.ViewSpec[object]{Name: "notes"})
		kind := pg.JSONB[map[string]int]()
		p := plan.DefineRead(view, "build", plan.Select(plan.Project(plan.Output(view, "value", kind), plan.JSONBuildObject(kind, plan.JSONPair(key.Expr(), number.Expr())))).From(f.src))
		for _, kind := range []q.PGType[json.RawMessage]{pg.JSON[json.RawMessage](), pg.JSONB[json.RawMessage]()} {
			doc := plan.Input("doc", kind)
			out := q.DefineView(f.reg, q.ViewSpec[raw]{Name: "regions" + string(rune('a'+f.serial))})
			f.serial++
			project := plan.DefineRead(out, "member", plan.Select(plan.Project(plan.Output(out, "value", kind), plan.JSONGet(doc.Expr(), key.Expr()))).From(f.src))
			require.NoError(f.t, f.run(f.t.Context(), func(ctx context.Context, s session) error {
				built, err := p.Bind(s).Find(ctx, plan.BindValue(key, "x"), plan.BindValue(number, int32(2)))
				require.NoError(f.t, err)
				require.Equal(f.t, map[string]int{"x": 2}, built[0].Value)
				got, err := project.Bind(s).Find(ctx, plan.BindValue(doc, json.RawMessage(`{"x":3}`)), plan.BindValue(key, "x"))
				require.NoError(f.t, err)
				require.Equal(f.t, new(json.RawMessage("3")), got[0].Value)
				return nil
			}))
		}
	})
}
