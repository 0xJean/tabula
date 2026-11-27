//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/netip"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/plan"
	"github.com/stretchr/testify/require"
)

type clockText string
type offsetClockText string

// A descriptor may register a scanner for the driver's time-of-day result.
// lib/pq represents 24:00 as midnight on January 2; pgx provides clock text.
type clockScanner struct {
	text   *string
	offset bool
}

func (c clockScanner) Scan(src any) error {
	switch v := src.(type) {
	case string:
		*c.text = v
	case []byte:
		*c.text = string(v)
	case time.Time:
		hms := v.Format("15:04:05.999999")
		if v.Day() == 2 {
			hms = "24:00:00"
		}
		if c.offset {
			hms += v.Format("-07:00:00")
		}
		*c.text = hms
	default:
		return fmt.Errorf("unsupported clock result %T", src)
	}
	return nil
}

func (c clockScanner) Value() (driver.Value, error) { return *c.text, nil }

func TestNativeTypeResultsAndVarcharAssignment(t *testing.T) {
	type item struct {
		ID       int64           `db:"id,key"`
		UUID     string          `db:"uid"`
		Amount   string          `db:"amount"`
		Clock    clockText       `db:"clock"`
		Zoned    offsetClockText `db:"zoned"`
		Interval sql.NullString  `db:"span"`
		Host     netip.Addr      `db:"host"`
		Prefix   netip.Prefix    `db:"prefix"`
		Doc      map[string]int  `db:"doc,json"`
		Short    string          `db:"short"`
	}
	const uuid = "00000000-0000-4000-8000-000000000001"
	const decimal = "123456789012345678901234567890.010000"
	db, pool := databases(t)
	schema := typesSchema(t, db)
	physical := schema + ".items"
	_, err := db.ExecContext(t.Context(), "CREATE TABLE "+physical+` (
id bigint PRIMARY KEY, uid uuid NOT NULL, amount numeric NOT NULL, clock time NOT NULL, zoned timetz NOT NULL,
span interval, host inet, prefix inet, doc jsonb, short varchar(2) NOT NULL)`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "INSERT INTO "+physical+` VALUES
(1, $1, $2, '24:00', '24:00+05:45', '1 mon 2 days 03:04:05.123456',
 '2001:db8::1', '192.0.2.7/24', '{"x":1}', 'ab')`, uuid, decimal)
	require.NoError(t, err)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{})
			q.RegisterType(reg, func(p *clockText) any { return clockScanner{text: (*string)(p)} })
			q.RegisterType(reg, func(p *offsetClockText) any { return clockScanner{text: (*string)(p), offset: true} })
			table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: physical, RelationScope: q.Global()})
			layout := plan.MapTable(table,
				plan.ColumnType("uid", pg.UUID[string]()), plan.ColumnType("amount", pg.Numeric[string]()),
				plan.ColumnType("clock", q.CatalogType[clockText]("pg_catalog", "time", nil)),
				plan.ColumnType("zoned", q.CatalogType[offsetClockText]("pg_catalog", "timetz", nil)),
				plan.ColumnType("span", q.CatalogType[sql.NullString]("pg_catalog", "interval", nil)),
				plan.ColumnType("host", pg.Inet()), plan.ColumnType("prefix", pg.InetPrefix()),
				plan.ColumnType("doc", pg.JSONB[map[string]int]()), plan.ColumnType("short", pg.Varchar(2)))
			src := layout.As("i")
			p := plan.DefineTableRows(src, "values", plan.RowsSpec{}, plan.SelectAll())
			short := plan.MappedColumn[string](layout, "short")
			id := plan.MappedColumn[int64](layout, "id")
			boundID, shortText := id.Input("id"), short.Input("value")
			lookup := plan.DefineExists(src, "lookup", plan.RowFilter{Where: plan.Eq(short.On(src.Rel()).Expr(), shortText.Expr())})
			edit := plan.DefineUpdateMany(src, "set_label", plan.MutationSpec{
				RowFilter: plan.RowFilter{Where: plan.Eq(id.On(src.Rel()).Expr(), boundID.Expr())},
			}, plan.RowSet{Name: "label", Assignments: []plan.RowAssignment{plan.Assign(short.On(src.Rel()), shortText.Expr())}})
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			over := plan.BindValue(shortText, "abcd")
			for _, style := range []string{"postgres", "iso_8601"} {
				require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
					_, err := s.Exec(ctx, "SET LOCAL IntervalStyle = "+style)
					require.NoError(t, err)
					v, err := p.Bind(s).One(ctx, plan.RowChoice{})
					require.NoError(t, err)
					require.Equal(t, uuid, v.UUID)
					require.Equal(t, decimal, v.Amount)
					require.True(t, v.Interval.Valid)
					require.Equal(t, netip.MustParseAddr("2001:db8::1"), v.Host)
					require.Equal(t, netip.MustParsePrefix("192.0.2.7/24"), v.Prefix)
					require.Equal(t, map[string]int{"x": 1}, v.Doc)
					require.Equal(t, "ab", v.Short)
					var correct bool
					require.NoError(t, s.Scan(ctx, `SELECT $1::time = '24:00'::time AND $2::timetz = '24:00+05:45'::timetz
AND $3::interval = '1 mon 2 days 03:04:05.123456'::interval`, []any{string(v.Clock), string(v.Zoned), v.Interval.String}, &correct))
					require.True(t, correct)
					found, err := lookup.Bind(s).Exists(ctx, nil, over)
					require.NoError(t, err)
					require.False(t, found)
					return nil
				}))
			}
			err := run(t.Context(), func(ctx context.Context, s session) error {
				_, err := edit.Bind(s).UpdateMany(ctx, "label", nil, plan.BindValue(boundID, 1), over)
				return err
			})
			require.Equal(t, "22001", q.SQLState(err))
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				row, err := p.Bind(s).One(ctx, plan.RowChoice{})
				require.NoError(t, err)
				require.Equal(t, "ab", row.Short)
				return nil
			}))
		})
	}
}
