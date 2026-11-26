package tabula

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type modeItem struct {
	ID       int64        `db:"id,key,serial"`
	Scope    customTenant `db:"scope_id,scope"`
	Label    string       `db:"label"`
	Rank     int64        `db:"rank"`
	Source   string       `db:"source,once"`
	Revision int          `db:"revision,version"`
	Removed  bool         `db:"removed,deleted"`
}

func TestWriteModeDefinitions(t *testing.T) {
	const name = "public.items"
	reg := NewRegistry(RegistryConfig[customTenant]{})
	base := TableSpec[modeItem, int64]{Name: name}
	for _, flags := range []struct{ readOnly, allowDelete bool }{{}, {true, false}, {false, true}} {
		spec := base
		spec.ReadOnly, spec.AllowDelete = flags.readOnly, flags.allowDelete
		legacy := Define(reg, spec).Definition()
		spec.WriteMode = WriteDefault
		require.Equal(t, legacy, Define(reg, spec).Definition())
		require.Contains(t, legacy.Statements, "get")
		if flags.readOnly {
			require.NotContains(t, legacy.Statements, "insert")
		} else {
			require.Contains(t, legacy.Statements["insert"], " RETURNING id")
			require.Contains(t, legacy.Statements["update"], "removed = $")
		}
		_, deletes := legacy.Statements["delete"]
		require.Equal(t, flags.allowDelete, deletes)
	}

	columns := []string{"revision", "rank"}
	table := Define(reg, TableSpec[modeItem, int64]{Name: name, WriteMode: WriteUpdateOnly, UpdateColumns: columns})
	columns[1] = "label"
	d := table.Definition()
	require.Equal(t, []string{"count", "get", "list", "lock", "unscoped_get", "unscoped_lock", "update"}, slices.Sorted(maps.Keys(d.Statements)))
	require.Equal(t, "UPDATE public.items SET rank = $3, revision = $4 WHERE id = $1 AND scope_id = $2 AND removed = FALSE AND revision = $4 - 1", d.Statements["update"])
	require.Equal(t, []DefinedPrivilege{
		{Table: name, Privilege: "SELECT", Columns: []string{"id", "scope_id", "label", "rank", "source", "revision", "removed"}},
		{Table: name, Privilege: "UPDATE", Columns: []string{"rank", "revision"}},
	}, d.Privileges)
	require.Empty(t, table.Insert())
	require.Empty(t, table.InsertColumns())
	sess := &fakeSession{tenant: "a", execRows: 1}
	rows := table.Bind(sess)
	row := modeItem{Scope: "a", Revision: 2}
	require.NoError(t, rows.Save(t.Context(), row))
	require.Equal(t, d.Statements["update"], sess.lastQuery)
	require.Len(t, sess.lastArgs, 4)
	require.NoError(t, rows.Set(t.Context(), row, "rank"))
	require.Contains(t, sess.lastQuery, "SET rank = $3 WHERE")
	require.NoError(t, rows.SetExpr(t.Context(), 7, Add("rank", 1)))
	require.ErrorIs(t, rows.SoftDelete(t.Context(), modeItem{Removed: true}), ErrOperationDenied)

	table = Define(reg, TableSpec[modeItem, int64]{Name: name, WriteMode: WriteInsertOnly})
	plan := table.DefineUpsert("append_once", UpsertSpec{})
	ins := "INSERT INTO public.items (scope_id, label, rank, source, revision, removed) VALUES ($1, $2, $3, $4, $5, $6)"
	require.Equal(t, map[string]string{"insert": ins, "append_once": ins + " ON CONFLICT DO NOTHING"}, table.Definition().Statements)
	require.Equal(t, []DefinedPrivilege{
		{Table: name, Privilege: "INSERT", Columns: []string{"scope_id", "label", "rank", "source", "revision", "removed"}},
		{Table: name, Privilege: "USAGE", Sequence: "id"},
	}, table.Definition().Privileges)
	rows = table.Bind(sess)
	row.ID = 42
	require.NoError(t, rows.Insert(t.Context(), &row))
	require.Equal(t, ins, sess.lastQuery)
	require.EqualValues(t, 42, row.ID)
	ok, err := rows.Upsert(t.Context(), plan, &row)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, table.Definition().Statements["append_once"], sess.lastQuery)

	type entry struct {
		ID        int64      `db:"id,key"`
		DeletedAt *time.Time `db:"deleted_at,deleted"`
	}
	explicit := Define(reg, TableSpec[entry, int64]{Name: name, WriteMode: WriteInsertOnly})
	require.Equal(t, []DefinedPrivilege{{Table: name, Privilege: "INSERT", Columns: []string{"id"}}}, explicit.Definition().Privileges)
	require.Equal(t, "INSERT INTO public.items (id) VALUES ($1)", explicit.Insert())
}

func TestInvalidWriteModes(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	for _, spec := range []TableSpec[modeItem, int64]{
		{WriteMode: 9}, {WriteMode: WriteUpdateOnly, ReadOnly: true}, {WriteMode: WriteInsertOnly, AllowDelete: true},
		{WriteMode: WriteUpdateOnly, AllowDelete: true}, {WriteMode: WriteInsertOnly, ReadOnly: true},
		{UpdateColumns: []string{"rank"}}, {WriteMode: WriteInsertOnly, UpdateColumns: []string{"rank"}},
		{WriteMode: WriteUpdateOnly, UpdateColumns: []string{}}, {WriteMode: WriteUpdateOnly, UpdateColumns: []string{"rank"}},
		{WriteMode: WriteInsertOnly, Order: "id"}, {WriteMode: WriteInsertOnly, GroupBy: []string{"rank"}},
		{WriteMode: WriteInsertOnly, Aggregates: ScalarAggregates{Max: []string{"rank"}}},
	} {
		spec.Name = "public.items"
		_, err := TryDefine(reg, spec)
		require.Error(t, err, "%+v", spec)
	}
	for _, col := range []string{"source", "scope_id", "id", "unknown", "revision"} {
		_, err := TryDefine(reg, TableSpec[modeItem, int64]{
			Name: "public.items", WriteMode: WriteUpdateOnly, UpdateColumns: []string{"revision", col},
		})
		require.Error(t, err, col)
	}
	type keyOnly struct {
		ID int64 `db:"id,key,serial"`
	}
	_, err := TryDefine(reg, TableSpec[keyOnly, int64]{Name: "public.entries", WriteMode: WriteUpdateOnly})
	require.ErrorContains(t, err, "updatable column")
	ids := Define(reg, TableSpec[keyOnly, int64]{Name: "public.entries", WriteMode: WriteInsertOnly})
	require.ErrorIs(t, ids.Bind(&fakeSession{}).CheckCopy(), ErrOperationDenied)
	only := ids.Definition()
	require.Equal(t, map[string]string{"insert": "INSERT INTO public.entries (id) VALUES (DEFAULT)"}, only.Statements)
	require.Equal(t, []DefinedPrivilege{
		{Table: "public.entries", Privilege: "INSERT", Columns: []string{"id"}},
		{Table: "public.entries", Privilege: "USAGE", Sequence: "id"},
	}, only.Privileges)
}

func TestUpdateOnlyDenialsBeforeIO(t *testing.T) {
	ctx := t.Context()
	table := Define(NewRegistry(RegistryConfig[customTenant]{}), TableSpec[modeItem, int64]{
		Name: "public.items", WriteMode: WriteUpdateOnly, UpdateColumns: []string{"rank", "revision"},
	})
	s := &fakeSession{}
	r := table.Bind(s)
	row := modeItem{}
	deny := func(err error) {
		t.Helper()
		require.ErrorIs(t, err, ErrOperationDenied)
		require.Empty(t, s.lastQuery)
	}
	deny(r.CheckCopy())
	deny(r.CheckWrite(nil))
	deny(r.Insert(ctx, nil))
	deny(r.InsertReturning(ctx, nil, "missing"))
	deny(r.InsertMany(ctx, nil))
	_, err := r.Upsert(ctx, nil, nil)
	deny(err)
	_, err = r.UpsertMany(ctx, nil, nil)
	deny(err)
	for _, spec := range []UpsertSpec{{}, {Conflict: []string{"id"}}, {Conflict: []string{"id"}, Update: []string{"rank"}}} {
		_, err = table.TryDefineUpsert("up", spec)
		deny(err)
	}
	for _, col := range []string{"id", "scope_id", "source", "label", "removed"} {
		deny(r.Set(ctx, row, col))
		_, err := r.SetWhere(ctx, row, []string{col}, "")
		deny(err)
		deny(r.SetExpr(ctx, 1, Add(col, 1)))
	}
	deny(r.SoftDelete(ctx, row))
	deny(r.Delete(ctx, 1))
	_, err = r.DeleteWhere(ctx, "")
	deny(err)
	deny(r.Exec(ctx, 0, "SELECT 1"))
	deny(r.Scan(ctx, "SELECT 1", nil))
	_, err = r.Select(ctx, "SELECT 1")
	deny(err)
	_, err = collect(r.SelectIter(ctx, "SELECT 1"))
	deny(err)
}

func TestInsertOnlyDenialsBeforeIO(t *testing.T) {
	ctx := t.Context()
	table := Define(NewRegistry(RegistryConfig[customTenant]{}), TableSpec[modeItem, int64]{Name: "public.entries", WriteMode: WriteInsertOnly})
	s := &fakeSession{}
	r := table.Bind(s)
	deny := func(err error) {
		t.Helper()
		require.ErrorIs(t, err, ErrOperationDenied)
		require.Empty(t, s.lastQuery)
	}
	row := modeItem{Scope: "other"}
	filter := func(*Filter) { t.Fatal("denied read evaluated filter") }
	_, err := r.Get(ctx, 1)
	deny(err)
	_, err = r.Lock(ctx, 1)
	deny(err)
	_, err = r.GetBy(ctx, "true")
	deny(err)
	_, err = r.LockBy(ctx, "true")
	deny(err)
	_, err = r.GetMany(ctx, nil)
	deny(err)
	_, err = r.LockMany(ctx, nil)
	deny(err)
	_, err = r.Exists(ctx, filter)
	deny(err)
	_, err = r.ExistsBy(ctx, "")
	deny(err)
	_, _, err = r.List(ctx, filter, Listing{Count: CountEstimate})
	deny(err)
	_, err = r.Find(ctx, filter, Listing{Lock: LockUpdate})
	deny(err)
	_, err = collect(r.Iter(ctx, filter, Listing{}))
	deny(err)
	_, err = r.ListKeyset(ctx, filter, KeysetListing{})
	deny(err)
	_, err = CountBy[int](ctx, r, "rank", filter)
	deny(err)
	_, _, err = Max[int](ctx, r, "rank", filter)
	deny(err)
	_, _, err = Min[int](ctx, r, "rank", filter)
	deny(err)
	_, _, err = Sum[int](ctx, r, "rank", filter)
	deny(err)
	_, _, err = Avg[float64](ctx, r, "rank", filter)
	deny(err)
	deny(r.InsertReturning(ctx, nil))
	deny(r.Save(ctx, row))
	deny(r.SaveReturning(ctx, nil, "missing"))
	_, err = r.UpdateWhere(ctx, row, "")
	deny(err)
	_, err = r.SetWhere(ctx, row, nil, "")
	deny(err)
	deny(r.Set(ctx, row))
	deny(r.SetExpr(ctx, 1))
	deny(r.SoftDelete(ctx, row))
	deny(r.Delete(ctx, 1))
	_, err = r.DeleteWhere(ctx, "")
	deny(err)
	deny(r.Exec(ctx, 1, "UPDATE public.entries SET rank = 0"))
	deny(r.Scan(ctx, "SELECT 1", nil))
	_, err = r.Select(ctx, "SELECT 1")
	deny(err)
	_, err = collect(r.SelectIter(ctx, "SELECT 1"))
	deny(err)
	for _, spec := range []UpsertSpec{{Conflict: []string{"id"}}, {Update: []string{"rank"}}, {Returning: []string{"id"}}, {Where: "true"}} {
		_, err := table.TryDefineUpsert("up", spec)
		deny(err)
	}
	other := Define(NewRegistry(RegistryConfig[customTenant]{}), TableSpec[modeItem, int64]{Name: "public.entries"})
	foreign := other.DefineUpsert("up", UpsertSpec{Conflict: []string{"id"}, Update: []string{"rank"}})
	_, err = r.Upsert(ctx, foreign, &modeItem{})
	deny(err)
	_, err = r.UpsertMany(ctx, foreign, nil)
	deny(err)
	require.NotContains(t, table.Definition().Statements, "up")
	require.ErrorIs(t, r.Insert(ctx, &row), ErrScopeMismatch)
	require.Empty(t, s.lastQuery)
}
