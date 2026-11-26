//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestReadListProductAndMappedPages(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			f := newListFixture(t, db, driver)
			other := f.layout.As("j")
			jid := q.FieldOf(other.Rel(), "id", q.Int8())
			type pair struct {
				Left  int64 `db:"left"`
				Right int64 `db:"right"`
			}
			out := q.DefineView(f.reg, q.ViewSpec[pair]{Name: "pairs"})
			product := q.DefineRead(out, "product", q.Select(q.Project(q.Output(out, "left", q.Int8()), f.id.Expr()), q.Project(q.Output(out, "right", q.Int8()), jid.Expr())).From(f.items).CrossJoin(other).
				WithList(q.ReadListSpec{Orders: []q.RowOrder{{Name: "ids", Terms: []q.RowTerm{q.Desc(jid), q.Asc(f.id)}}}, Pages: q.RowPages{Unpaged: true, Keyset: true, MaxLimit: 5}, ExactCount: true}))
			selection := q.SelectColumns(f.label.Column(), f.id.Column())
			mapped := q.DefineTableRead(f.items, "mapped", selection, q.Select().WithList(q.ReadListSpec{Orders: []q.RowOrder{{Name: "label", Terms: []q.RowTerm{q.Asc(f.label).NullsFirst(), q.Asc(f.id)}}}, Pages: q.RowPages{Unpaged: true, Keyset: true, MaxLimit: 2}, ExactCount: true}))
			lock := q.DefineTableRead(f.items, "locked", q.SelectColumns(f.id.Column()), q.Select().Lock(q.ForRows(q.ForKeyShare, f.items.Rel(), q.Wait)).WithList(q.ReadListSpec{Orders: []q.RowOrder{{Name: "id", Terms: []q.RowTerm{q.Asc(f.id)}}}, Pages: q.RowPages{Keyset: true, MaxLimit: 2}}))
			_, err := db.ExecContext(t.Context(), "GRANT UPDATE(label) ON "+f.schema+".items TO "+f.role)
			require.NoError(t, err)

			type compositeRow struct {
				Region int32 `db:"region,key"`
				ID     int64 `db:"id,key"`
			}
			_, err = db.ExecContext(t.Context(), "CREATE TABLE "+f.schema+".groups (region int, id bigint, PRIMARY KEY(region,id)); INSERT INTO "+f.schema+".groups VALUES (1,1),(2,1),(1,2); GRANT SELECT(region,id) ON "+f.schema+".groups TO "+f.role)
			require.NoError(t, err)
			compositeSource := q.MapTable(q.DefinePlanSource(f.reg, q.TableSpec[compositeRow, compositeRow]{Name: f.schema + ".groups", RelationScope: q.Global()})).As("g")
			region, cid := q.FieldOf(compositeSource.Rel(), "region", q.Int4()), q.FieldOf(compositeSource.Rel(), "id", q.Int8())
			composite := q.DefineTableRead(compositeSource, "composite", q.SelectAll(), q.Select().WithList(q.ReadListSpec{Orders: []q.RowOrder{{Name: "key", Terms: []q.RowTerm{q.Asc(cid), q.Desc(region)}}}, Pages: q.RowPages{Keyset: true, MaxLimit: 1}, ExactCount: true}))
			verifyListSnapshot(t, db, f)
			err = runRowPlan(t, db, pool, driver, f.reg.SQLAuthority(), f.role, 1, q.LogConfig{}, func(ctx context.Context, s q.Session[itemScope]) error {
				all, total, err := product.Bind(s).List(ctx, q.RowChoice{Order: "ids"})
				require.NoError(t, err)
				require.EqualValues(t, 16, total)
				var got []pair
				choice := q.RowChoice{Order: "ids", Page: q.FirstPage, Limit: 5}
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
					choice.Page, choice.After = q.AfterPage, cursor
				}
				require.Equal(t, all, got)
				page, n, err := mapped.Bind(s).List(ctx, q.RowChoice{Order: "label", Page: q.FirstPage, Limit: 2})
				require.NoError(t, err)
				require.EqualValues(t, 4, n)
				require.Equal(t, []listItem{{ID: 3}, {ID: 1, Label: new("a")}}, page)
				cursor, err := mapped.CursorFrom("label", page[1])
				require.NoError(t, err)
				page, err = mapped.Bind(s).Page(ctx, q.RowChoice{Order: "label", Page: q.AfterPage, Limit: 2, After: cursor})
				require.NoError(t, err)
				require.Equal(t, []listItem{{ID: 2, Label: new("b")}, {ID: 4, Label: new("b")}}, page)
				locked, err := lock.Bind(s).Page(ctx, q.RowChoice{Order: "id", Page: q.FirstPage, Limit: 2})
				require.NoError(t, err)
				require.Len(t, locked, 2)
				cursor, err = lock.CursorFrom("id", locked[1])
				require.NoError(t, err)
				locked, err = lock.Bind(s).Page(ctx, q.RowChoice{Order: "id", Page: q.AfterPage, Limit: 2, After: cursor})
				require.NoError(t, err)
				require.EqualValues(t, 3, locked[0].ID)
				_, err = lock.Bind(s).Count(ctx, nil)
				require.ErrorIs(t, err, q.ErrOperationDenied)
				_, _, err = lock.Bind(s).List(ctx, q.RowChoice{Order: "id", Page: q.FirstPage, Limit: 2})
				require.ErrorIs(t, err, q.ErrOperationDenied)

				var keys []compositeRow
				keyChoice := q.RowChoice{Order: "key", Page: q.FirstPage, Limit: 1}
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
					keyChoice.Page, keyChoice.After = q.AfterPage, after
				}
				require.Equal(t, []compositeRow{{2, 1}, {1, 1}, {1, 2}}, keys)
				return nil
			})
			require.NoError(t, err)
		})
	}
}
