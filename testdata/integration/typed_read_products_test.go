//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/plan"
	"github.com/stretchr/testify/require"
)

func TestReadListProductAndMappedPages(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			f := newListFixture(t, db, driver)
			other := f.layout.As("j")
			jid := plan.FieldOf(other.Rel(), "id", plan.Int8())
			type pair struct {
				Left  int64 `db:"left"`
				Right int64 `db:"right"`
			}
			out := q.DefineView(f.reg, q.ViewSpec[pair]{Name: "pairs"})
			product := plan.DefineRead(out, "product", plan.Select(plan.Project(plan.Output(out, "left", plan.Int8()), f.id.Expr()), plan.Project(plan.Output(out, "right", plan.Int8()), jid.Expr())).From(f.items).CrossJoin(other).
				WithList(plan.ReadListSpec{Orders: []plan.RowOrder{{Name: "ids", Terms: []plan.RowTerm{plan.Desc(jid), plan.Asc(f.id)}}}, Pages: plan.RowPages{Unpaged: true, Keyset: true, MaxLimit: 5}, ExactCount: true}))
			selection := plan.SelectColumns(f.label.Column(), f.id.Column())
			mapped := plan.DefineTableRead(f.items, "mapped", selection, plan.Select().WithList(plan.ReadListSpec{Orders: []plan.RowOrder{{Name: "label", Terms: []plan.RowTerm{plan.Asc(f.label).NullsFirst(), plan.Asc(f.id)}}}, Pages: plan.RowPages{Unpaged: true, Keyset: true, MaxLimit: 2}, ExactCount: true}))
			lock := plan.DefineTableRead(f.items, "locked", plan.SelectColumns(f.id.Column()), plan.Select().Lock(plan.ForRows(plan.ForKeyShare, f.items.Rel(), plan.Wait)).WithList(plan.ReadListSpec{Orders: []plan.RowOrder{{Name: "id", Terms: []plan.RowTerm{plan.Asc(f.id)}}}, Pages: plan.RowPages{Keyset: true, MaxLimit: 2}}))
			_, err := db.ExecContext(t.Context(), "GRANT UPDATE(label) ON "+f.schema+".items TO "+f.role)
			require.NoError(t, err)

			type compositeRow struct {
				Region int32 `db:"region,key"`
				ID     int64 `db:"id,key"`
			}
			_, err = db.ExecContext(t.Context(), "CREATE TABLE "+f.schema+".groups (region int, id bigint, PRIMARY KEY(region,id)); INSERT INTO "+f.schema+".groups VALUES (1,1),(2,1),(1,2); GRANT SELECT(region,id) ON "+f.schema+".groups TO "+f.role)
			require.NoError(t, err)
			compositeSource := plan.MapTable(q.DefinePlanSource(f.reg, q.TableSpec[compositeRow, compositeRow]{Name: f.schema + ".groups", RelationScope: q.Global()})).As("g")
			region, cid := plan.FieldOf(compositeSource.Rel(), "region", plan.Int4()), plan.FieldOf(compositeSource.Rel(), "id", plan.Int8())
			composite := plan.DefineTableRead(compositeSource, "composite", plan.SelectAll(), plan.Select().WithList(plan.ReadListSpec{Orders: []plan.RowOrder{{Name: "key", Terms: []plan.RowTerm{plan.Asc(cid), plan.Desc(region)}}}, Pages: plan.RowPages{Keyset: true, MaxLimit: 1}, ExactCount: true}))
			verifyListSnapshot(t, db, f)
			err = runRowPlan(t, db, pool, driver, f.reg.SQLAuthority(), f.role, 1, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				all, total, err := product.Bind(s).List(ctx, plan.RowChoice{Order: "ids"})
				require.NoError(t, err)
				require.EqualValues(t, 16, total)
				var got []pair
				choice := plan.RowChoice{Order: "ids", Page: plan.FirstPage, Limit: 5}
				for range 6 {
					page, n, err := product.Bind(s).List(ctx, choice)
					require.NoError(t, err)
					require.EqualValues(t, 16, n)
					if len(page) == 0 {
						break
					}
					got = append(got, page...)
					cursor, err := product.CursorFrom("ids", page[len(page)-1])
					require.NoError(t, err)
					choice.Page, choice.After = plan.AfterPage, cursor
				}
				require.Equal(t, all, got)
				page, n, err := mapped.Bind(s).List(ctx, plan.RowChoice{Order: "label", Page: plan.FirstPage, Limit: 2})
				require.NoError(t, err)
				require.EqualValues(t, 4, n)
				require.Equal(t, []listItem{{ID: 3}, {ID: 1, Label: new("a")}}, page)
				cursor, err := mapped.CursorFrom("label", page[1])
				require.NoError(t, err)
				page, err = mapped.Bind(s).Page(ctx, plan.RowChoice{Order: "label", Page: plan.AfterPage, Limit: 2, After: cursor})
				require.NoError(t, err)
				require.Equal(t, []listItem{{ID: 2, Label: new("b")}, {ID: 4, Label: new("b")}}, page)
				locked, err := lock.Bind(s).Page(ctx, plan.RowChoice{Order: "id", Page: plan.FirstPage, Limit: 2})
				require.NoError(t, err)
				require.Len(t, locked, 2)
				cursor, err = lock.CursorFrom("id", locked[1])
				require.NoError(t, err)
				locked, err = lock.Bind(s).Page(ctx, plan.RowChoice{Order: "id", Page: plan.AfterPage, Limit: 2, After: cursor})
				require.NoError(t, err)
				require.EqualValues(t, 3, locked[0].ID)
				_, err = lock.Bind(s).Count(ctx, nil)
				require.ErrorIs(t, err, q.ErrOperationDenied)
				_, _, err = lock.Bind(s).List(ctx, plan.RowChoice{Order: "id", Page: plan.FirstPage, Limit: 2})
				require.ErrorIs(t, err, q.ErrOperationDenied)

				var keys []compositeRow
				keyChoice := plan.RowChoice{Order: "key", Page: plan.FirstPage, Limit: 1}
				for range 5 {
					page, total, err := composite.Bind(s).List(ctx, keyChoice)
					require.NoError(t, err)
					require.EqualValues(t, 3, total)
					if len(page) == 0 {
						break
					}
					keys = append(keys, page...)
					after, err := composite.CursorFrom("key", page[0])
					require.NoError(t, err)
					keyChoice.Page, keyChoice.After = plan.AfterPage, after
				}
				require.Equal(t, []compositeRow{{2, 1}, {1, 1}, {1, 2}}, keys)
				return nil
			})
			require.NoError(t, err)
		})
	}
}
