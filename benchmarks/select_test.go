// Package benchmarks compares the cost of reading the same PostgreSQL rows.
package benchmarks

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/stdsql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type item struct {
	ID     int64  `db:"id"`
	Code   string `db:"code"`
	Amount int64  `db:"amount"`
}

// Each adapter executes one parameterized SELECT and materializes the result.
func readSQL(ctx context.Context, db *sql.DB, q string) ([]item, error) {
	rows, err := db.QueryContext(ctx, q, 1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []item
	for rows.Next() {
		var v item
		if err := rows.Scan(&v.ID, &v.Code, &v.Amount); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func readPgx(ctx context.Context, p *pgxpool.Pool, q string) ([]item, error) {
	rows, err := p.Query(ctx, q, 1)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[item])
}

func readSQLX(ctx context.Context, db *sqlx.DB, q string) ([]item, error) {
	var out []item
	err := db.SelectContext(ctx, &out, q, 1)
	return out, err
}

func readGORM(ctx context.Context, db *gorm.DB, q string) ([]item, error) {
	var out []item
	err := db.WithContext(ctx).Raw(q, 1).Scan(&out).Error
	return out, err
}

func readTabula(ctx context.Context, r *tabula.ViewRows[item, struct{}], q string) ([]item, error) {
	return r.Select(ctx, q, 1)
}

func BenchmarkSelect100(b *testing.B) {
	ctx := b.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	if dsn == "" {
		b.Skip("set TABULA_TEST_DSN to a disposable PostgreSQL URL")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	table := fmt.Sprintf("tabula_compare_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, "CREATE TABLE "+table+"(id bigint PRIMARY KEY, code text NOT NULL, amount bigint NOT NULL)"); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _, _ = db.Exec("DROP TABLE " + table) })
	if _, err := db.ExecContext(ctx, "INSERT INTO "+table+" SELECT i, 'code-' || i, i*10 FROM generate_series(1,100) i"); err != nil {
		b.Fatal(err)
	}
	q := "SELECT id, code, amount FROM " + table + " WHERE id >= $1 ORDER BY id"
	gq := strings.Replace(q, "$1", "?", 1)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(pool.Close)
	xdb := sqlx.NewDb(db, "pgx")
	gdb, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		b.Fatal(err)
	}
	direct, err := stdsql.Direct("pgx", dsn, stdsql.NoScopePolicy())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = direct.Close() })
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	view := tabula.DefineView(reg, tabula.ViewSpec[item]{Name: table}).Bind(direct.Session(struct{}{}))

	for _, tc := range []struct {
		name string
		read func() ([]item, error)
	}{
		{"database_sql", func() ([]item, error) { return readSQL(ctx, db, q) }},
		{"pgx", func() ([]item, error) { return readPgx(ctx, pool, q) }},
		{"sqlx", func() ([]item, error) { return readSQLX(ctx, xdb, q) }},
		{"gorm", func() ([]item, error) { return readGORM(ctx, gdb, gq) }},
		{"tabula", func() ([]item, error) { return readTabula(ctx, view, q) }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			want := make([]item, 100)
			for i := range want {
				n := int64(i + 1)
				want[i] = item{ID: n, Code: fmt.Sprintf("code-%d", n), Amount: n * 10}
			}
			warm, err := tc.read()
			if err != nil || !slices.Equal(warm, want) {
				b.Fatalf("warm-up: %d rows, err=%v", len(warm), err)
			}
			b.ReportAllocs()
			for b.Loop() {
				out, err := tc.read()
				if err != nil || len(out) != 100 {
					b.Fatalf("read: %d rows, err=%v", len(out), err)
				}
			}
		})
	}
}
