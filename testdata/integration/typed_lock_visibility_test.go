//go:build integration

package integration_test

import (
	"database/sql"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/plan"
	"github.com/stretchr/testify/require"
)

func addStatusLock[V any](t *testing.T, db *sql.DB, f *effectFixture, name, sqlType string, kind q.PGType[V], live, removed V, explicitLive bool) {
	t.Helper()
	type record struct {
		ID     int64 `db:"id,key"`
		Status V     `db:"status,deleted"`
	}
	table := f.schema + "." + name
	ctx := t.Context()
	_, err := db.ExecContext(ctx, "CREATE TABLE "+table+" (id bigint PRIMARY KEY, status "+sqlType+" NOT NULL, note text)")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO "+table+" (id, status) VALUES (1,$1), (2,$2)", live, removed)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "GRANT SELECT(id, status), UPDATE(note) ON "+table+" TO "+f.role)
	require.NoError(t, err)
	visibility := &q.SoftDelete{DeletedValue: removed}
	if explicitLive {
		visibility.AliveValue = live
	}
	desc := q.Define(f.reg, q.TableSpec[record, int64]{Name: table, RelationScope: q.Global(), SoftDelete: visibility})
	rel := plan.FromTable(desc, "r", q.Global())
	source := plan.RowsFrom(desc, rel, plan.FieldOf(rel, "status", kind).Column())
	f.statusLocks[name] = plan.DefineRows(f.view, name, source, plan.RowsSpec{Lock: plan.ForRows(plan.ForShare, rel, plan.Wait)},
		plan.Project(plan.Output(f.view, "id", plan.Int8()), plan.FieldOf(rel, "id", plan.Int8()).Expr()))
}
