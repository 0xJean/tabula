//go:build integration

package pgxdriver_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
)

func TestPgxRunnerAndTabulaIntegration(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn, "TABULA_TEST_DSN is required for integration tests")
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	schema := fmt.Sprintf("tabula_pgx_it_%d", time.Now().UnixNano())
	_, err = pool.Exec(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `CREATE TABLE `+schema+`.items (
		id bigserial PRIMARY KEY,
		tenant_id text NOT NULL,
		code text NOT NULL,
		tags text[],
		version integer NOT NULL DEFAULT 1
	)`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })

	type tenant string
	type item struct {
		ID      int64    `db:"id,key,serial"`
		Tenant  tenant   `db:"tenant_id,scope"`
		Code    string   `db:"code"`
		Tags    []string `db:"tags,array"`
		Version int      `db:"version,version"`
	}

	reg := tabula.NewRegistry(tabula.RegistryConfig[tenant]{
		ScopeID:     func(s tenant) any { return string(s) },
		ArrayColumn: pgxdriver.ArrayColumn,
	})
	tbl := tabula.Define(reg, tabula.TableSpec[item, int64]{Name: schema + ".items", Order: "id ASC"})
	roTbl := tabula.Define(reg, tabula.TableSpec[item, int64]{Name: schema + ".items", ReadOnly: true})

	runner, err := pgxdriver.NewRunner(pool, pgxdriver.ScopePolicy[tenant]{
		ManagedVars: []string{"app.current_tenant"},
		ScopeVars: func(scope tenant) map[string]string {
			return map[string]string{"app.current_tenant": string(scope)}
		},
	}, pgxdriver.RunnerOptions{})
	require.NoError(t, err)

	require.NoError(t, runner.Within(ctx, "t1", func(ctx context.Context, s *pgxdriver.Session[tenant]) error {
		r := tbl.Bind(s)
		row := item{Tenant: "t1", Code: "alpha", Tags: []string{"core", "featured"}, Version: 1}
		require.NoError(t, r.Insert(ctx, &row))
		require.NotZero(t, row.ID)

		got, err := r.Get(ctx, row.ID)
		require.NoError(t, err)
		require.Equal(t, []string{"core", "featured"}, got.Tags)

		// Missing row maps pgx.ErrNoRows -> sql.ErrNoRows -> nil, nil.
		missing, err := r.Get(ctx, 999999)
		require.NoError(t, err)
		require.Nil(t, missing)

		// Savepoint rollback keeps outer row intact.
		spErr := s.Savepoint(ctx, func(ctx context.Context, child *pgxdriver.Session[tenant]) error {
			tmp := item{Tenant: "t1", Code: "rollback_me", Version: 1}
			require.NoError(t, tbl.Bind(child).Insert(ctx, &tmp))
			return errors.New("abort savepoint")
		})
		require.ErrorContains(t, spErr, "abort savepoint")

		many, err := r.GetMany(ctx, []int64{row.ID})
		require.NoError(t, err)
		require.Len(t, many, 1)

		// CopyFrom rejects ReadOnly table even for an empty batch.
		_, err = pgxdriver.CopyFrom(ctx, s, roTbl, nil)
		require.ErrorIs(t, err, tabula.ErrReadOnly)

		// CopyFrom bulk-inserts rows via PostgreSQL COPY protocol.
		copied, err := pgxdriver.CopyFrom(ctx, s, tbl, []item{
			{Tenant: "t1", Code: "beta", Tags: []string{"core", "draft"}, Version: 1},
			{Tenant: "t1", Code: "gamma", Tags: []string{"core"}, Version: 1},
		})
		require.NoError(t, err)
		require.Equal(t, int64(2), copied)

		all, err := r.Find(ctx, nil, tabula.Listing{})
		require.NoError(t, err)
		require.Len(t, all, 3)
		require.Equal(t, []string{"core", "draft"}, all[1].Tags)
		return nil
	}))

	// WithinReadOnly enforces ErrReadOnly on derived writes, CopyFrom, and PG 25006.
	err = runner.WithinReadOnly(ctx, "t1", func(ctx context.Context, s *pgxdriver.Session[tenant]) error {
		require.True(t, s.ReadOnly())
		_, err := pgxdriver.CopyFrom(ctx, s, tbl, nil)
		require.ErrorIs(t, err, tabula.ErrReadOnly)
		require.ErrorIs(t, tbl.Bind(s).Insert(ctx, &item{Tenant: "t1", Code: "ro", Version: 1}), tabula.ErrReadOnly)
		_, rawErr := s.Exec(ctx, "INSERT INTO "+schema+".items (tenant_id, code, version) VALUES ($1, $2, 1)", "t1", "ro")
		return rawErr
	})
	require.ErrorIs(t, err, tabula.ErrReadOnly)
	require.Equal(t, "25006", tabula.SQLState(err))
}

func BenchmarkPGX_CopyFrom1000(b *testing.B) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	if dsn == "" {
		b.Skip("TABULA_TEST_DSN not set")
	}
	ctx := b.Context()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(pool.Close)

	schema := fmt.Sprintf("tabula_pgx_bench_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })
	if _, err := pool.Exec(ctx, `CREATE TABLE `+schema+`.items (
		id bigserial PRIMARY KEY,
		code text NOT NULL,
		name text NOT NULL,
		amount bigint NOT NULL,
		version integer NOT NULL DEFAULT 1
	)`); err != nil {
		b.Fatal(err)
	}

	type benchItem struct {
		ID      int64  `db:"id,key,serial"`
		Code    string `db:"code"`
		Name    string `db:"name"`
		Amount  int64  `db:"amount"`
		Version int    `db:"version,version"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	tbl := tabula.Define(reg, tabula.TableSpec[benchItem, int64]{Name: schema + ".items"})

	batch := make([]benchItem, 1000)
	for i := range batch {
		batch[i] = benchItem{Code: fmt.Sprintf("c-%d", i), Name: "bulk", Amount: int64(i), Version: 1}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			b.Fatal(err)
		}
		sess := pgxdriver.NewSession(tx, struct{}{}, pgxdriver.NoScopePolicy())
		n, err := pgxdriver.CopyFrom(ctx, sess, tbl, batch)
		if err != nil || n != 1000 {
			_ = tx.Rollback(ctx)
			b.Fatalf("n=%d err=%v", n, err)
		}
		_ = tx.Rollback(ctx)
	}
}

func TestDirectAndCopyTypes(t *testing.T) {
	type scope string
	type enabled bool
	type row struct {
		ID      int64        `db:"id,key"`
		Tenant  scope        `db:"tenant,scope"`
		Counts  []int32      `db:"counts,array"`
		Weights []float64    `db:"weights,array"`
		Enabled []enabled    `db:"enabled,array"`
		Address netip.Addr   `db:"address"`
		Prefix  netip.Prefix `db:"prefix"`
	}
	ctx := t.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	name := fmt.Sprintf("pgx_types_%d", time.Now().UnixNano())
	_, err = db.Exec(ctx, "CREATE TABLE "+name+" (id bigint PRIMARY KEY, tenant text, counts integer[], weights float8[], enabled bool[], address inet, prefix cidr)")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), "DROP TABLE "+name) })
	reg := tabula.NewRegistry(tabula.RegistryConfig[scope]{ArrayColumn: pgxdriver.ArrayColumn})
	tbl := tabula.Define(reg, tabula.TableSpec[row, int64]{Name: name})
	input := row{ID: 1, Tenant: "one", Counts: []int32{}, Weights: []float64{0.125}, Enabled: []enabled{true, false}, Address: netip.MustParseAddr("2001:db8::5"), Prefix: netip.MustParsePrefix("10.0.0.0/8")}
	policy := pgxdriver.ScopePolicy[scope]{}
	runner, err := pgxdriver.NewRunner(db, policy, pgxdriver.RunnerOptions{})
	require.NoError(t, err)
	require.NoError(t, runner.Within(ctx, "one", func(ctx context.Context, s *pgxdriver.Session[scope]) error {
		wrong := input
		wrong.ID = 2
		wrong.Tenant = "two"
		_, err := pgxdriver.CopyFrom(ctx, s, tbl, []row{input, wrong})
		require.ErrorIs(t, err, tabula.ErrScopeMismatch)
		_, err = pgxdriver.CopyFrom(ctx, s, tbl, []row{input})
		return err
	}))
	denied := errors.New("application read policy")
	policy.IsUnscoped = func(s scope) bool { return s == "all" }
	policy.GuardQuery = func(query string) error {
		if query == "SELECT 42" {
			return denied
		}
		return nil
	}
	ro, err := pgxdriver.Direct(ctx, dsn, policy, func(cfg *pgxpool.Config) {
		cfg.MaxConns = 4
		cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "off" // Direct must re-enforce "on"
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ro.Close() })
	require.Equal(t, int32(4), ro.Stat().MaxConns())
	s := ro.Session("one")
	require.Equal(t, scope("one"), s.Scope())
	require.False(t, s.Unscoped())
	require.True(t, ro.Session("all").Unscoped())
	got, err := tbl.Bind(s).Get(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, &input, got)
	other, err := tbl.Bind(ro.Session("two")).Get(ctx, 1)
	require.NoError(t, err)
	require.Nil(t, other)
	require.ErrorIs(t, tbl.Bind(s).Insert(ctx, &input), tabula.ErrReadOnly)
	var enabledOnServer bool
	require.NoError(t, s.Scan(ctx, "SELECT current_setting('transaction_read_only')::bool", nil, &enabledOnServer))
	require.True(t, enabledOnServer)
	require.ErrorIs(t, s.Scan(ctx, "SELECT 42", nil), denied)
	require.ErrorIs(t, s.Scan(ctx, "SELECT 1; SELECT 2", nil), tabula.ErrReadOnly)
	_, err = s.Exec(ctx, "SELECT 1")
	require.ErrorIs(t, err, tabula.ErrReadOnly)
	require.ErrorIs(t, s.Returning(ctx, "SELECT 1", nil), tabula.ErrReadOnly)
	lock := "SELECT id FROM " + name + " FOR UPDATE"
	err = s.Scan(ctx, lock, nil, new(int64))
	require.ErrorIs(t, err, tabula.ErrReadOnly)
	require.Equal(t, "25006", tabula.SQLState(err))
	err = s.Each(ctx, lock, nil, func(scan func(...any) error) error { return scan(new(int64)) })
	require.ErrorIs(t, err, tabula.ErrReadOnly)
	require.Equal(t, "25006", tabula.SQLState(err))
	_, err = pgxdriver.Direct(ctx, dsn, pgxdriver.ScopePolicy[scope]{ManagedVars: []string{"app.scope"}})
	require.Error(t, err)
}
