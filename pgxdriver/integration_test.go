//go:build integration

package pgxdriver_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
		Tenant  tenant   `db:"tenant_id,tenant"`
		Code    string   `db:"code"`
		Tags    []string `db:"tags,array"`
		Version int      `db:"version,version"`
	}

	reg := tabula.NewRegistry(tabula.RegistryConfig[tenant]{
		TenantID:    func(s tenant) any { return string(s) },
		ArrayColumn: pgxdriver.ArrayColumn,
	})
	tbl := tabula.Define(reg, tabula.Table[item, int64, tenant]{Name: schema + ".items", Order: "id ASC"})

	runner := pgxdriver.NewRunner(pool, pgxdriver.ScopePolicy[tenant]{
		SetupTx: func(ctx context.Context, tx pgx.Tx, scope tenant) error {
			return pgxdriver.SetLocalConfig(ctx, tx, "app.current_tenant", string(scope))
		},
	})

	require.NoError(t, runner.Within(ctx, "t1", func(ctx context.Context, s *pgxdriver.Session[tenant]) error {
		r := tbl.Bind(s)
		row := item{Tenant: "t1", Code: "btc", Tags: []string{"l1", "pow"}, Version: 1}
		require.NoError(t, r.Insert(ctx, &row))
		require.NotZero(t, row.ID)

		got, err := r.Get(ctx, "t1", row.ID)
		require.NoError(t, err)
		require.Equal(t, []string{"l1", "pow"}, got.Tags)

		// Missing row maps pgx.ErrNoRows -> sql.ErrNoRows -> nil, nil.
		missing, err := r.Get(ctx, "t1", 999999)
		require.NoError(t, err)
		require.Nil(t, missing)

		// Savepoint rollback keeps outer row intact.
		spErr := s.Savepoint(ctx, func(ctx context.Context, child *pgxdriver.Session[tenant]) error {
			tmp := item{Tenant: "t1", Code: "rollback_me", Version: 1}
			require.NoError(t, tbl.Bind(child).Insert(ctx, &tmp))
			return errors.New("abort savepoint")
		})
		require.ErrorContains(t, spErr, "abort savepoint")

		many, err := r.GetMany(ctx, "t1", []int64{row.ID})
		require.NoError(t, err)
		require.Len(t, many, 1)

		// CopyFrom bulk-inserts rows via PostgreSQL COPY protocol.
		copied, err := pgxdriver.CopyFrom(ctx, s, tbl, []item{
			{Tenant: "t1", Code: "eth", Tags: []string{"l1", "pos"}, Version: 1},
			{Tenant: "t1", Code: "sol", Tags: []string{"l1"}, Version: 1},
		})
		require.NoError(t, err)
		require.Equal(t, int64(2), copied)

		all, err := r.Find(ctx, "t1", nil, tabula.Listing{})
		require.NoError(t, err)
		require.Len(t, all, 3)
		require.Equal(t, []string{"l1", "pos"}, all[1].Tags)
		return nil
	}))
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
	tbl := tabula.Define(reg, tabula.Table[benchItem, int64, struct{}]{Name: schema + ".items"})

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
