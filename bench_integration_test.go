//go:build integration

package tabula_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/stdsql"
)

type pgBenchRow struct {
	ID      int64  `db:"id,key,serial"`
	Code    string `db:"code"`
	Name    string `db:"name"`
	Amount  int64  `db:"amount"`
	Version int    `db:"version,version"`
}

func setupBenchDB(b *testing.B) (*sql.DB, string) {
	b.Helper()
	dsn := os.Getenv("TABULA_TEST_DSN")
	if dsn == "" {
		b.Skip("TABULA_TEST_DSN not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })

	schema := fmt.Sprintf("tabula_bench_%d", time.Now().UnixNano())
	ctx := b.Context()
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })

	if _, err := db.ExecContext(ctx, `CREATE TABLE `+schema+`.items (
		id bigserial PRIMARY KEY,
		code text NOT NULL,
		name text NOT NULL,
		amount bigint NOT NULL,
		version integer NOT NULL DEFAULT 1
	)`); err != nil {
		b.Fatal(err)
	}
	for i := range 100 {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO `+schema+`.items (code, name, amount, version) VALUES ($1, $2, $3, 1)`,
			fmt.Sprintf("code-%03d", i), fmt.Sprintf("Item %d", i), int64(i*100)); err != nil {
			b.Fatal(err)
		}
	}
	return db, schema
}

func BenchmarkPG_Select100_RawSQL(b *testing.B) {
	db, schema := setupBenchDB(b)
	ctx := b.Context()
	query := "SELECT id, code, name, amount, version FROM " + schema + ".items ORDER BY id ASC"
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			b.Fatal(err)
		}
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			_ = tx.Rollback()
			b.Fatal(err)
		}
		var out []pgBenchRow
		for rows.Next() {
			var r pgBenchRow
			if err := rows.Scan(&r.ID, &r.Code, &r.Name, &r.Amount, &r.Version); err != nil {
				_ = rows.Close()
				_ = tx.Rollback()
				b.Fatal(err)
			}
			out = append(out, r)
		}
		_ = rows.Close()
		_ = tx.Commit()
		if len(out) != 100 {
			b.Fatalf("expected 100, got %d", len(out))
		}
	}
}

func BenchmarkPG_Select100_TabulaStdSQL(b *testing.B) {
	db, schema := setupBenchDB(b)
	ctx := b.Context()
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	tbl := tabula.Define(reg, tabula.TableSpec[pgBenchRow, int64]{Name: schema + ".items", Order: "id ASC"})
	runner := stdsql.NewRunner(db, stdsql.NoScopePolicy())
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		err := runner.WithinReadOnly(ctx, struct{}{}, func(ctx context.Context, s *stdsql.Session[struct{}]) error {
			out, err := tbl.Bind(s).Find(ctx, nil, tabula.Listing{})
			if err != nil || len(out) != 100 {
				return fmt.Errorf("len=%d err=%w", len(out), err)
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPG_InsertMany1000_TabulaStdSQL(b *testing.B) {
	db, schema := setupBenchDB(b)
	ctx := b.Context()
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	tbl := tabula.Define(reg, tabula.TableSpec[pgBenchRow, int64]{Name: schema + ".items"})
	runner := stdsql.NewRunner(db, stdsql.NoScopePolicy())

	batch := make([]pgBenchRow, 1000)
	for i := range batch {
		batch[i] = pgBenchRow{Code: fmt.Sprintf("b-%d", i), Name: "bulk", Amount: int64(i), Version: 1}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			b.Fatal(err)
		}
		sess := stdsql.NewSession(tx, struct{}{}, stdsql.NoScopePolicy())
		if err := tbl.Bind(sess).InsertMany(ctx, batch); err != nil {
			_ = tx.Rollback()
			b.Fatal(err)
		}
		_ = tx.Rollback()
	}
	_ = runner
}
