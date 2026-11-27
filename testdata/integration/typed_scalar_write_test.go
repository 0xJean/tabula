//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

type scalarWriteItem struct {
	ID     int64            `db:"id,key"`
	Source *int32           `db:"source"`
	Target int32            `db:"target"`
	Stamp  time.Time        `db:"stamp"`
	UID    *string          `db:"uid"`
	Doc    *json.RawMessage `db:"doc,json"`
	Values []int32          `db:"members,array"`
}

func TestScalarAssignmentMinimumGrants(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx := t.Context()
			schema := typesSchema(t, db)
			tableName, role := schema+".items", fmt.Sprintf("scope_editor_%d", time.Now().UnixNano())
			exec := func(query string) { t.Helper(); _, err := db.ExecContext(ctx, query); require.NoError(t, err) }
			exec("CREATE ROLE " + role)
			t.Cleanup(func() { _, err := db.Exec("DROP OWNED BY " + role + "; DROP ROLE " + role); require.NoError(t, err) })
			exec("CREATE TABLE " + tableName + ` (id bigint PRIMARY KEY, source int4, target int4 NOT NULL, stamp timestamptz NOT NULL, uid uuid, doc jsonb, members int4[]);
INSERT INTO ` + tableName + ` VALUES (1,10,0,now(),gen_random_uuid(),'{"x":1}',ARRAY[1,2,1]),(2,NULL,0,now(),gen_random_uuid(),'{}',ARRAY[]::int4[])`)
			exec("GRANT USAGE ON SCHEMA " + schema + " TO " + role)
			exec("GRANT SELECT(id,source,doc,members), UPDATE(target,stamp,uid,doc,members) ON " + tableName + " TO " + role)
			cfg := q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL, ArrayColumn: pqdriver.ArrayColumn}
			if driver == "pq" {
				cfg.ArrayArg = pqdriver.Array
			}
			reg := q.NewRegistry(cfg)
			table := q.DefinePlanSource(reg, q.TableSpec[scalarWriteItem, int64]{Name: tableName, RelationScope: q.Global(), WriteMode: q.WriteUpdateOnly, UpdateColumns: []string{"target", "stamp", "uid", "doc", "members"}})
			layout := tplan.MapTable(table, tplan.ColumnType("uid", pg.UUID[string]()), tplan.ColumnType("doc", pg.JSONB[json.RawMessage]()), tplan.ColumnType("members", pg.ArrayOf(pg.Int4())))
			src := layout.As("i")
			id := tplan.MappedColumn[int64](layout, "id").On(src.Rel())
			source, target := tplan.MappedColumn[int32](layout, "source").On(src.Rel()), tplan.MappedColumn[int32](layout, "target").On(src.Rel())
			stamp := tplan.MappedColumn[time.Time](layout, "stamp").On(src.Rel())
			uid := tplan.MappedColumn[string](layout, "uid").On(src.Rel())
			doc := tplan.MappedColumn[json.RawMessage](layout, "doc").On(src.Rel())
			values := tplan.MappedColumn[[]int32](layout, "members").On(src.Rel())
			which, value, key := tplan.Input("id", pg.Int8()), tplan.Input("value", pg.Int4()), tplan.Input("key", pg.Text())
			fallback := tplan.Coalesce(source.Expr(), value.Expr())
			plan := tplan.DefineUpdateMany(src, "change", tplan.MutationSpec{RowFilter: tplan.RowFilter{Where: tplan.Eq(id.Expr(), which.Expr())}},
				tplan.RowSet{Name: "slot", Assignments: []tplan.RowAssignment{tplan.Assign(target, value.Expr())}},
				tplan.RowSet{Name: "conditional", Assignments: []tplan.RowAssignment{tplan.Assign(target, tplan.Case(tplan.When(tplan.IsNull(source.Expr()), value.Expr())).Else(fallback))}},
				tplan.RowSet{Name: "delta", Assignments: []tplan.RowAssignment{tplan.Assign(target, tplan.Plus(fallback, value.Expr()))}},
				tplan.RowSet{Name: "clock", Assignments: []tplan.RowAssignment{tplan.Assign(stamp, tplan.Now())}},
				tplan.RowSet{Name: "uuid", Assignments: []tplan.RowAssignment{tplan.Assign(uid, tplan.GenRandomUUID())}},
				tplan.RowSet{Name: "json", Assignments: []tplan.RowAssignment{tplan.Assign(doc, tplan.JSONBDeleteKey(doc.Expr(), key.Expr()))}},
				tplan.RowSet{Name: "array", Assignments: []tplan.RowAssignment{tplan.Assign(values, tplan.ArrayRemove(values.Expr(), value.Expr()))}},
			)
			defs := reg.Defined()
			inv := sqlcheck.SQLInventory{Sites: []sqlcheck.SQLSite{{ID: "change", Method: "DefineUpdateMany", Kind: "typed_rows", Definition: tableName, QueryKey: "change"}}}
			work, err := inv.BindRolesWithDefinitions(map[string]sqlcheck.SQLBinding{"change": {Roles: []string{role}}}, defs, nil)
			require.NoError(t, err)
			needs, err := work.Readiness(role)
			require.NoError(t, err)
			missing, err := sqlcheck.CheckReadiness(ctx, db, role, needs)
			require.NoError(t, err)
			require.Empty(t, missing)
			findings, err := sqlcheck.VerifyRoleStatements(ctx, db, work)
			require.NoError(t, err)
			require.Empty(t, findings)
			for _, shape := range table.Definition().StatementShapes {
				for _, grant := range shape.Privileges {
					if grant.Privilege == "SELECT" {
						require.NotContains(t, grant.Columns, "target")
						require.NotContains(t, grant.Columns, "uid")
						require.NotContains(t, grant.Columns, "stamp")
					}
				}
			}
			run := authorizedEffectRunner(t, driver, db, pool, role, reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			generate := tplan.DefineUpdateMany(src, "generate", tplan.MutationSpec{RowFilter: tplan.RowFilter{Where: tplan.Ge(id.Expr(), which.Expr())}},
				tplan.RowSet{Name: "uuid", Assignments: []tplan.RowAssignment{tplan.Assign(uid, tplan.NullIf(tplan.GenRandomUUID(), tplan.TypedNull(pg.UUID[string]())))}})
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				n, err := generate.Bind(s).UpdateMany(ctx, "uuid", nil, tplan.BindValue(which, int64(1)))
				require.NoError(t, err)
				require.EqualValues(t, 2, n)
				return nil
			}))
			var distinctUUIDs int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(DISTINCT uid) FROM "+tableName).Scan(&distinctUUIDs))
			require.Equal(t, 2, distinctUUIDs)
			change := func(set string, bindings ...tplan.Binding) error {
				return run(ctx, func(ctx context.Context, s session) error {
					n, err := plan.Bind(s).UpdateMany(ctx, set, nil, bindings...)
					if err == nil {
						require.EqualValues(t, 1, n)
					}
					return err
				})
			}
			for _, idValue := range []int64{1, 2} {
				args := []tplan.Binding{tplan.BindValue(which, idValue), tplan.BindValue(value, int32(3))}
				require.NoError(t, change("slot", args...))
				require.NoError(t, change("conditional", args...))
				require.NoError(t, change("delta", args...))
				var got int32
				require.NoError(t, db.QueryRowContext(ctx, "SELECT target FROM "+tableName+" WHERE id=$1", idValue).Scan(&got))
				want := int32(13)
				if idValue == 2 {
					want = 6
				}
				require.Equal(t, want, got)
				require.NoError(t, change("clock", tplan.BindValue(which, idValue)))
				require.NoError(t, change("uuid", tplan.BindValue(which, idValue)))
				require.NoError(t, change("json", tplan.BindValue(which, idValue), tplan.BindValue(key, "x")))
				require.NoError(t, change("array", tplan.BindValue(which, idValue), tplan.BindValue(value, int32(1))))
			}
			exec("REVOKE SELECT(source) ON " + tableName + " FROM " + role)
			require.NoError(t, change("slot", tplan.BindValue(which, int64(1)), tplan.BindValue(value, int32(7))))
			require.Equal(t, "42501", q.SQLState(change("conditional", tplan.BindValue(which, int64(1)), tplan.BindValue(value, int32(7)))))
			missing, err = sqlcheck.CheckReadiness(ctx, db, role, needs)
			require.NoError(t, err)
			require.NotEmpty(t, missing)
			exec("GRANT SELECT(source) ON " + tableName + " TO " + role)
			exec("REVOKE UPDATE(target) ON " + tableName + " FROM " + role)
			require.Equal(t, "42501", q.SQLState(change("slot", tplan.BindValue(which, int64(1)), tplan.BindValue(value, int32(7)))))
			exec("GRANT UPDATE(target) ON " + tableName + " TO " + role)
			ro := authorizedEffectRunner(t, driver, db, pool, role, reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			err = ro(ctx, func(ctx context.Context, s session) error {
				_, err := plan.Bind(s).UpdateMany(ctx, "uuid", nil, tplan.BindValue(which, int64(1)))
				return err
			})
			require.ErrorIs(t, err, q.ErrReadOnly)
			_, err = plan.Bind(effectDirect(t, driver, reg.SQLAuthority())).UpdateMany(ctx, "uuid", nil, tplan.BindValue(which, int64(1)))
			require.ErrorIs(t, err, q.ErrReadOnly)
		})
	}
}
