//go:build integration

package stdsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/0xJean/tabula/stdsql"
	"github.com/stretchr/testify/require"
)

func TestScopedReadsAndBatchWrites(t *testing.T) {
	type tenant string
	type job struct {
		ID       int64      `db:"id,key"`
		Tenant   tenant     `db:"tenant,tenant"`
		Attempts int        `db:"attempts"`
		Updated  *time.Time `db:"updated"`
	}
	ctx := t.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	name := fmt.Sprintf("jobs_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE TABLE "+name+"(id bigint PRIMARY KEY, tenant text NOT NULL, attempts integer NOT NULL, updated timestamptz)")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE " + name) })
	reg := tabula.NewRegistry(tabula.RegistryConfig[tenant]{})
	tbl := tabula.Define(reg, tabula.TableSpec[job, int64]{Name: name, Order: "id", AllowDelete: true})
	router := stdsql.NewRunner(db, stdsql.ScopePolicy[tenant]{IsPlatform: func(s tenant) bool { return s == "platform" }})
	plan := tbl.DefineUpsert("up", tabula.UpsertSpec{Conflict: []string{"id"}, Update: []string{"attempts"}, Returning: []string{"updated"}})
	skip := tbl.DefineUpsert("skip", tabula.UpsertSpec{Conflict: []string{"id"}})
	within := func(scope tenant, fn func(*tabula.Rows[job, int64, tenant]) error) error {
		return router.Within(ctx, scope, func(_ context.Context, s *stdsql.Session[tenant]) error { return fn(tbl.Bind(s)) })
	}
	require.NoError(t, within("platform", func(r *tabula.Rows[job, int64, tenant]) error {
		return r.InsertMany(ctx, []job{{ID: 1, Tenant: "a"}, {ID: 2, Tenant: "a"}, {ID: 3, Tenant: "b"}})
	}))
	require.NoError(t, within("a", func(r *tabula.Rows[job, int64, tenant]) error {
		other, err := r.Get(ctx, 3)
		require.NoError(t, err)
		require.Nil(t, other)
		require.ErrorIs(t, r.Delete(ctx, 3), tabula.ErrConflict)
		wrong := job{ID: 3, Tenant: "b"}
		require.ErrorIs(t, r.Insert(ctx, &wrong), tabula.ErrScopeMismatch)
		require.ErrorIs(t, r.Save(ctx, wrong), tabula.ErrScopeMismatch)
		n, err := r.UpsertMany(ctx, plan, []job{{ID: 1, Tenant: "a", Attempts: 2}, {ID: 3, Tenant: "a", Attempts: 9}})
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
		n, err = r.UpsertMany(ctx, skip, []job{{ID: 1, Tenant: "a"}, {ID: 4, Tenant: "a"}})
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
		require.NoError(t, r.SetExpr(ctx, 1, tabula.Add("attempts", 3), tabula.SetNow("updated")))
		got, err := r.Get(ctx, 1)
		require.NoError(t, err)
		require.Equal(t, 5, got.Attempts)
		require.NotNil(t, got.Updated)
		require.ErrorIs(t, r.SetExpr(ctx, 3, tabula.Add("attempts", 1)), tabula.ErrRowCount)
		require.Error(t, r.SetExpr(ctx, 1, tabula.Add("id", 1)))
		return nil
	}))

	// Hold a lock in a fresh transaction. No earlier write locks this row.
	require.NoError(t, within("a", func(r *tabula.Rows[job, int64, tenant]) error {
		var claimed int64
		for job, err := range r.Iter(ctx, nil, tabula.Listing{Lock: tabula.LockUpdate, Size: 1}) {
			require.NoError(t, err)
			claimed = job.ID
			break
		}
		require.EqualValues(t, 1, claimed)
		// The first cursor is closed. Its row lock is held until commit.
		err = within("a", func(worker *tabula.Rows[job, int64, tenant]) error {
			page, err := worker.ListKeyset(ctx, nil, tabula.KeysetListing{Lock: tabula.LockSkipLocked, Limit: 1})
			require.NoError(t, err)
			require.EqualValues(t, 2, page.Items[0].ID)
			_, err = worker.Find(ctx, nil, tabula.Listing{Size: 1, Lock: tabula.LockNoWait})
			return err
		})
		require.Equal(t, "55P03", tabula.SQLState(err))
		got, err := r.Get(ctx, 1)
		require.NoError(t, err)
		require.NotNil(t, got)
		return nil
	}))

	direct, err := stdsql.Direct("postgres", dsn, stdsql.ScopePolicy[tenant]{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = direct.Close() })
	sess := direct.Session("a")
	r := tbl.Bind(sess)
	got, err := r.Get(ctx, 3)
	require.NoError(t, err)
	require.Nil(t, got)
	require.ErrorIs(t, r.SetExpr(ctx, 1, tabula.Add("attempts", 1)), tabula.ErrReadOnly)
	_, err = r.Lock(ctx, 1)
	require.ErrorIs(t, err, tabula.ErrReadOnly)
	_, _, err = r.List(ctx, nil, tabula.Listing{Lock: tabula.LockShare})
	require.ErrorIs(t, err, tabula.ErrReadOnly)
	var on bool
	require.NoError(t, sess.Scan(ctx, "SELECT current_setting('transaction_read_only')::bool", nil, &on))
	require.True(t, on)
	var n int
	err = sess.Scan(ctx, "WITH changed AS (UPDATE "+name+" SET attempts = 0 RETURNING id) SELECT count(*) FROM changed", nil, &n)
	require.ErrorIs(t, err, tabula.ErrReadOnly)
	require.Equal(t, "25006", tabula.SQLState(err))
	_, err = stdsql.Direct("postgres", dsn, stdsql.ScopePolicy[tenant]{SetupTx: func(context.Context, *sql.Tx, tenant) error { return nil }})
	require.Error(t, err)
}

func TestArrayAndNetworkProjection(t *testing.T) {
	type flag bool
	type Projection struct {
		Flags    []flag       `db:"flags,array"`
		Numbers  []int32      `db:"nums,array"`
		Rates    []float64    `db:"rates,array"`
		Host     netip.Addr   `db:"host"`
		Network  netip.Prefix `db:"network"`
		Optional *netip.Addr  `db:"optional"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{ArrayColumn: pqdriver.ArrayColumn})
	view := tabula.DefineView(reg, tabula.ViewSpec[Projection]{Name: "types"})
	pool, err := stdsql.Direct("postgres", os.Getenv("TABULA_TEST_DSN"), stdsql.NoScopePolicy())
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	r := view.Bind(pool.Session(struct{}{}))
	input := Projection{Flags: []flag{true, false}, Numbers: []int32{}, Rates: []float64{1.25}, Host: netip.MustParseAddr("192.0.2.1"), Network: netip.MustParsePrefix("2001:db8::/48")}
	// Value bindings below exercise the same converters that Table writes use.
	type values struct {
		ID int64 `db:"id,key,serial"`
		Projection
	}
	table := tabula.Define(reg, tabula.TableSpec[values, int64]{Name: "values_test"})
	args := table.Bind(pool.Session(struct{}{})).Args(values{Projection: input})
	query := "SELECT $1::boolean[], $2::integer[], $3::float8[], $4::inet, $5::cidr, $6::inet"
	var seen int
	for row, err := range r.Iter(t.Context(), query, args...) {
		require.NoError(t, err)
		require.Equal(t, input, row)
		seen++
	}
	require.Equal(t, 1, seen)
	seen = 0
	for _, err := range r.Iter(t.Context(), "SELECT NULL,NULL,NULL,'192.0.2.1/24'::inet,NULL,NULL") {
		require.Error(t, err)
		seen++
	}
	require.Equal(t, 1, seen)
}

func TestUpsertManyRollsBackEarlierChunks(t *testing.T) {
	type item struct {
		ID       int64 `db:"id,key"`
		Quantity int   `db:"qty"`
	}
	ctx := t.Context()
	db, err := sql.Open("postgres", os.Getenv("TABULA_TEST_DSN"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	name := fmt.Sprintf("chunked_items_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE TABLE "+name+" (id bigint PRIMARY KEY, qty integer CHECK(qty >= 0))")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec("DROP TABLE " + name) })
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table := tabula.Define(reg, tabula.TableSpec[item, int64]{Name: name})
	plan := table.DefineUpsert("upsert", tabula.UpsertSpec{Conflict: []string{"id"}, Update: []string{"qty"}})
	batch := make([]item, 32768) // Two parameters per row exceed the protocol limit by one.
	for i := range batch {
		batch[i] = item{ID: int64(i), Quantity: 1}
	}
	runner := stdsql.NewRunner(db, stdsql.NoScopePolicy())
	save := func() error {
		return runner.Within(ctx, struct{}{}, func(ctx context.Context, s *stdsql.Session[struct{}]) error {
			n, err := table.Bind(s).UpsertMany(ctx, plan, batch)
			if err == nil {
				require.EqualValues(t, len(batch), n)
			}
			return err
		})
	}
	batch[len(batch)-1].Quantity = -1
	require.ErrorIs(t, save(), tabula.ErrCheck)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+name).Scan(&count))
	require.Zero(t, count)
	batch[len(batch)-1].Quantity = 1
	require.NoError(t, save())
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+name).Scan(&count))
	require.Equal(t, len(batch), count)
}
