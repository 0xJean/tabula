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
			layout := q.MapTable(table, q.ColumnType("uid", pg.UUID[string]()), q.ColumnType("doc", pg.JSONB[json.RawMessage]()), q.ColumnType("members", pg.ArrayOf(pg.Int4())))
			src := layout.As("i")
			id := q.MappedColumn[int64](layout, "id").On(src.Rel())
			source, target := q.MappedColumn[int32](layout, "source").On(src.Rel()), q.MappedColumn[int32](layout, "target").On(src.Rel())
			stamp := q.MappedColumn[time.Time](layout, "stamp").On(src.Rel())
			uid := q.MappedColumn[string](layout, "uid").On(src.Rel())
			doc := q.MappedColumn[json.RawMessage](layout, "doc").On(src.Rel())
			values := q.MappedColumn[[]int32](layout, "members").On(src.Rel())
			which, value, key := q.Input("id", pg.Int8()), q.Input("value", pg.Int4()), q.Input("key", pg.Text())
			fallback := q.Coalesce(source.Expr(), value.Expr())
			plan := q.DefineUpdateMany(src, "change", q.MutationSpec{RowFilter: q.RowFilter{Where: q.Eq(id.Expr(), which.Expr())}},
				q.RowSet{Name: "slot", Assignments: []q.RowAssignment{q.Assign(target, value.Expr())}},
				q.RowSet{Name: "conditional", Assignments: []q.RowAssignment{q.Assign(target, q.Case(q.When(q.IsNull(source.Expr()), value.Expr())).Else(fallback))}},
				q.RowSet{Name: "delta", Assignments: []q.RowAssignment{q.Assign(target, q.Plus(fallback, value.Expr()))}},
				q.RowSet{Name: "clock", Assignments: []q.RowAssignment{q.Assign(stamp, q.Now())}},
				q.RowSet{Name: "uuid", Assignments: []q.RowAssignment{q.Assign(uid, q.GenRandomUUID())}},
				q.RowSet{Name: "json", Assignments: []q.RowAssignment{q.Assign(doc, q.JSONBDeleteKey(doc.Expr(), key.Expr()))}},
				q.RowSet{Name: "array", Assignments: []q.RowAssignment{q.Assign(values, q.ArrayRemove(values.Expr(), value.Expr()))}},
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
			generate := q.DefineUpdateMany(src, "generate", q.MutationSpec{RowFilter: q.RowFilter{Where: q.Ge(id.Expr(), which.Expr())}},
				q.RowSet{Name: "uuid", Assignments: []q.RowAssignment{q.Assign(uid, q.NullIf(q.GenRandomUUID(), q.TypedNull(pg.UUID[string]())))}})
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				n, err := generate.Bind(s).UpdateMany(ctx, "uuid", nil, q.BindValue(which, int64(1)))
				require.NoError(t, err)
				require.EqualValues(t, 2, n)
				return nil
			}))
			var distinctUUIDs int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(DISTINCT uid) FROM "+tableName).Scan(&distinctUUIDs))
			require.Equal(t, 2, distinctUUIDs)
			change := func(set string, bindings ...q.Binding) error {
				return run(ctx, func(ctx context.Context, s session) error {
					n, err := plan.Bind(s).UpdateMany(ctx, set, nil, bindings...)
					if err == nil {
						require.EqualValues(t, 1, n)
					}
					return err
				})
			}
			for _, idValue := range []int64{1, 2} {
				args := []q.Binding{q.BindValue(which, idValue), q.BindValue(value, int32(3))}
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
				require.NoError(t, change("clock", q.BindValue(which, idValue)))
				require.NoError(t, change("uuid", q.BindValue(which, idValue)))
				require.NoError(t, change("json", q.BindValue(which, idValue), q.BindValue(key, "x")))
				require.NoError(t, change("array", q.BindValue(which, idValue), q.BindValue(value, int32(1))))
			}
			exec("REVOKE SELECT(source) ON " + tableName + " FROM " + role)
			require.NoError(t, change("slot", q.BindValue(which, int64(1)), q.BindValue(value, int32(7))))
			require.Equal(t, "42501", q.SQLState(change("conditional", q.BindValue(which, int64(1)), q.BindValue(value, int32(7)))))
			missing, err = sqlcheck.CheckReadiness(ctx, db, role, needs)
			require.NoError(t, err)
			require.NotEmpty(t, missing)
			exec("GRANT SELECT(source) ON " + tableName + " TO " + role)
			exec("REVOKE UPDATE(target) ON " + tableName + " FROM " + role)
			require.Equal(t, "42501", q.SQLState(change("slot", q.BindValue(which, int64(1)), q.BindValue(value, int32(7)))))
			exec("GRANT UPDATE(target) ON " + tableName + " TO " + role)
			ro := authorizedEffectRunner(t, driver, db, pool, role, reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			err = ro(ctx, func(ctx context.Context, s session) error {
				_, err := plan.Bind(s).UpdateMany(ctx, "uuid", nil, q.BindValue(which, int64(1)))
				return err
			})
			require.ErrorIs(t, err, q.ErrReadOnly)
			_, err = plan.Bind(effectDirect(t, driver, reg.SQLAuthority())).UpdateMany(ctx, "uuid", nil, q.BindValue(which, int64(1)))
			require.ErrorIs(t, err, q.ErrReadOnly)
		})
	}
}
