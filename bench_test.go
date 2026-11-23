package tabula

import (
	"context"
	"testing"
)

type benchRow struct {
	ID        int64        `db:"id,key,serial"`
	Tenant    customTenant `db:"tenant_id,tenant"`
	Code      string       `db:"code"`
	Name      string       `db:"name"`
	Amount    int64        `db:"amount"`
	CreatedBy string       `db:"created_by,once"`
	Version   int          `db:"version,version"`
}

type benchSession struct {
	rows [][]any
}

func (b *benchSession) Tenant() customTenant { return "t1" }
func (b *benchSession) Platform() bool       { return false }
func (b *benchSession) Exec(_ context.Context, _ string, _ ...any) (int64, error) {
	return 1, nil
}
func (b *benchSession) Scan(_ context.Context, _ string, _ []any, dest ...any) error {
	*dest[0].(*int64) = 42
	*dest[1].(*customTenant) = "t1"
	*dest[2].(*string) = "BTC"
	*dest[3].(*string) = "Bitcoin"
	*dest[4].(*int64) = 100000
	*dest[5].(*string) = "alice"
	*dest[6].(*int) = 1
	return nil
}
func (b *benchSession) Returning(ctx context.Context, q string, args []any, dest ...any) error {
	return b.Scan(ctx, q, args, dest...)
}
func (b *benchSession) Each(_ context.Context, _ string, _ []any, decode func(func(...any) error) error) error {
	scan := func(dest ...any) error {
		*dest[0].(*int64) = 42
		*dest[1].(*customTenant) = "t1"
		*dest[2].(*string) = "BTC"
		*dest[3].(*string) = "Bitcoin"
		*dest[4].(*int64) = 100000
		*dest[5].(*string) = "alice"
		*dest[6].(*int) = 1
		return nil
	}
	for range b.rows {
		if err := decode(scan); err != nil {
			return err
		}
	}
	return nil
}

func BenchmarkFilterBuild(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		f := NewFilter("t1")
		f.And("status = ?", "active")
		f.AndIf(true, "code = ?", "BTC")
		f.In("chain", "ETH", "SOL", "ARB")
		f.Range("amount", int64(10), int64(1000))
		_ = f.Where() + f.Page(20, 40)
		_ = f.Args()
	}
}

func BenchmarkOrderSpecResolve(b *testing.B) {
	spec := MustOrderSpec(OrderSpec{
		Columns:          map[string]string{"id": "id", "createdAt": "created_at", "code": "code"},
		DefaultColumn:    "created_at",
		DefaultDirection: "DESC",
		TieBreaker:       "id ASC",
	})
	b.ReportAllocs()
	for b.Loop() {
		_ = spec.Resolve("createdAt", "asc")
	}
}

func BenchmarkRowsGet_Tabula(b *testing.B) {
	reg := NewRegistry(RegistryConfig[customTenant]{
		TenantID: func(s customTenant) any { return string(s) },
	})
	tbl := Define(reg, TableSpec[benchRow, int64]{Name: "public.bench_items"})
	rows := tbl.Bind(&benchSession{})
	ctx := b.Context()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		got, err := rows.Get(ctx, 42)
		if err != nil || got.ID != 42 {
			b.Fatal(err)
		}
	}
}

func BenchmarkRowsGet_RawManualScan(b *testing.B) {
	sess := &benchSession{}
	ctx := b.Context()
	const query = "SELECT id, tenant_id, code, name, amount, created_by, version FROM public.bench_items WHERE id = $1 AND tenant_id = $2"
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var row benchRow
		if err := sess.Scan(ctx, query, []any{int64(42), "t1"},
			&row.ID, &row.Tenant, &row.Code, &row.Name, &row.Amount, &row.CreatedBy, &row.Version); err != nil || row.ID != 42 {
			b.Fatal(err)
		}
	}
}

func BenchmarkRowsSelect100_Tabula(b *testing.B) {
	reg := NewRegistry(RegistryConfig[customTenant]{
		TenantID: func(s customTenant) any { return string(s) },
	})
	tbl := Define(reg, TableSpec[benchRow, int64]{Name: "public.bench_items"})
	sess := &benchSession{rows: make([][]any, 100)}
	rows := tbl.Bind(sess)
	ctx := b.Context()
	const q = "SELECT id, tenant_id, code, name, amount, created_by, version FROM public.bench_items WHERE tenant_id = $1"
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		out, err := rows.Select(ctx, q, "t1")
		if err != nil || len(out) != 100 {
			b.Fatal(err)
		}
	}
}

func BenchmarkRowsSelect100_RawManualScan(b *testing.B) {
	sess := &benchSession{rows: make([][]any, 100)}
	ctx := b.Context()
	const q = "SELECT id, tenant_id, code, name, amount, created_by, version FROM public.bench_items WHERE tenant_id = $1"
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var out []benchRow
		err := sess.Each(ctx, q, []any{"t1"}, func(scan func(...any) error) error {
			var row benchRow
			if err := scan(&row.ID, &row.Tenant, &row.Code, &row.Name, &row.Amount, &row.CreatedBy, &row.Version); err != nil {
				return err
			}
			out = append(out, row)
			return nil
		})
		if err != nil || len(out) != 100 {
			b.Fatal(err)
		}
	}
}
