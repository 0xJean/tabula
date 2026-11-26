//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	t "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/stretchr/testify/require"
)

type aggregateNullable struct {
	IDs []*int32 `db:"ids,array"`
}

func TestAggregateNullableInputsBothDrivers(test *testing.T) {
	db, pool := databases(test)
	for _, driver := range []string{"stdsql", "pgx"} {
		test.Run(driver, func(test *testing.T) {
			ns := fmt.Sprintf("nullable_aggregate_%d", time.Now().UnixNano())
			role := ns + "_reader"
			exec := func(sql string) {
				test.Helper()
				_, err := db.ExecContext(test.Context(), sql)
				require.NoError(test, err)
			}
			exec("CREATE ROLE " + role)
			test.Cleanup(func() { _, err := db.Exec("DROP ROLE " + role); require.NoError(test, err) })
			exec("CREATE SCHEMA " + ns)
			test.Cleanup(func() { _, err := db.Exec("DROP SCHEMA " + ns + " CASCADE"); require.NoError(test, err) })
			exec("CREATE TABLE " + ns + ".items(id integer PRIMARY KEY, label text, rank integer NOT NULL, flag boolean)")
			exec("INSERT INTO " + ns + ".items VALUES(1,'a',3,true),(2,'b',2,false)")
			exec("CREATE TABLE " + ns + ".notes(id integer PRIMARY KEY, item_id integer NOT NULL, rank integer NOT NULL)")
			exec("INSERT INTO " + ns + ".notes VALUES(11,1,3)")
			exec("GRANT USAGE ON SCHEMA " + ns + " TO " + role)
			exec("GRANT SELECT(id) ON " + ns + ".items TO " + role)
			exec("GRANT SELECT(id,item_id) ON " + ns + ".notes TO " + role)
			cfg := t.RegistryConfig[itemScope]{Deny: t.DenyLegacySQL, ArrayColumn: pgxdriver.ArrayColumn}
			if driver == "stdsql" {
				cfg.ArrayColumn, cfg.ArrayArg = pqdriver.ArrayColumn, pqdriver.Array
			}
			reg := t.NewRegistry(cfg)
			items := t.DefinePlanSource(reg, t.TableSpec[aggregateItem, int32]{Name: ns + ".items", RelationScope: t.Global()})
			notes := t.DefinePlanSource(reg, t.TableSpec[aggregateNote, int32]{Name: ns + ".notes", RelationScope: t.Global()})
			left, right := t.MapTable(items).As("i"), t.MapTable(notes).As("n")
			id, nid, parent := t.FieldOf(left.Rel(), "id", pg.Int4()), t.FieldOf(right.Rel(), "id", pg.Int4()), t.FieldOf(right.Rel(), "item_id", pg.Int4())
			out := t.DefineView(reg, t.ViewSpec[aggregateNullable]{Name: "entries"})
			kind := pg.ArrayOfNullable(pg.Int4())
			floor := t.Input("floor", pg.Int4())
			aggregate := t.AggregateOrder(t.ArrayAggAs(kind, nid.Expr()), t.Asc(id))
			plan := t.DefineRead(out, "ids", t.Select(t.Project(t.Output(out, "ids", kind), aggregate)).From(left).LeftJoin(right, t.Eq(id.Expr(), parent.Expr())).Where(t.Ge(id.Expr(), floor.Expr())))
			verifyAggregateDefinitions(test, db, role, reg)
			require.NoError(test, runRowPlan(test, db, pool, driver, reg.SQLAuthority(), role, 1, t.LogConfig{}, func(ctx context.Context, s t.Session[itemScope]) error {
				got, err := plan.Bind(s).Find(ctx, t.BindValue(floor, 1))
				require.NoError(test, err)
				require.Equal(test, []aggregateNullable{{IDs: []*int32{new(int32(11)), nil}}}, got)
				got, err = plan.Bind(s).Find(ctx, t.BindValue(floor, 2))
				require.NoError(test, err)
				require.Equal(test, []aggregateNullable{{IDs: []*int32{nil}}}, got)
				got, err = plan.Bind(s).Find(ctx, t.BindValue(floor, 3))
				require.NoError(test, err)
				require.Equal(test, []aggregateNullable{{}}, got)
				return nil
			}))
		})
	}
}
