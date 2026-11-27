//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/netip"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/stretchr/testify/require"
)

type inputText struct {
	Value *string `db:"value"`
}

type typeFixture struct {
	t      *testing.T
	db     *sql.DB
	schema string
	view   *q.View[inputText, struct{}]
}

func typesSchema(t *testing.T, db *sql.DB) string {
	t.Helper()
	schema := fmt.Sprintf("typed_values_%d", time.Now().UnixNano())
	_, err := db.ExecContext(t.Context(), "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec("DROP SCHEMA " + schema + " CASCADE")
		require.NoError(t, err)
	})
	return schema
}

type inputCase[V any] struct {
	value V
	want  *string
}

func valueCase[V any](value V, want string) inputCase[V] {
	return inputCase[V]{value, &want}
}

// Check the actual prepared parameter OID, then execute the registered call
// through each driver. PostgreSQL renders the value in the current session's
// output style, and parses it back for a database equality comparison.
func typeRoundTrip[V any](f typeFixture, name, sqlType string, kind q.PGType[V], cases ...inputCase[V]) callback {
	t := f.t
	t.Helper()
	qualified := f.schema + ".read_" + name
	_, err := f.db.ExecContext(t.Context(), "CREATE FUNCTION "+qualified+"("+sqlType+") RETURNS text LANGUAGE SQL STABLE AS 'SELECT $1::text'")
	require.NoError(t, err)
	input := tplan.Input("value", kind)
	fn := tplan.DeclareRoutine(tplan.RoutineSpec[string]{
		Schema: f.schema, Name: "read_" + name, Result: pg.Text(), Cardinality: tplan.RoutineOne, ReadOnly: true, Nullable: true,
	}, tplan.Parameter("value", kind))
	plan := tplan.DefineRoutine(f.view, name, fn, tplan.Arg(tplan.FormalOf(fn, "value", kind), input.Expr()))
	tx, err := f.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	// PREPARE survives rollback when a pooled connection is reused.
	stmt, err := tx.PrepareContext(t.Context(), f.view.Definition().Statements[name+".call"])
	require.NoError(t, err)
	var oidMatches bool
	err = tx.QueryRowContext(t.Context(), `SELECT EXISTS (
SELECT 1 FROM pg_prepared_statements
WHERE statement=$1 AND parameter_types=ARRAY[pg_catalog.to_regtype($2)])`,
		f.view.Definition().Statements[name+".call"], sqlType).Scan(&oidMatches)
	require.NoError(t, err)
	require.True(t, oidMatches, "parameter identity for %s", name)
	require.NoError(t, stmt.Close())

	compareType := sqlType
	if sqlType == "json" {
		compareType = "jsonb"
	}
	query := "SELECT $1::" + compareType + " IS NOT DISTINCT FROM $2::" + compareType
	return func(ctx context.Context, s session) error {
		for i, c := range cases {
			binding := tplan.BindValue(input, c.value)
			got, err := plan.Bind(s).One(ctx, binding)
			require.NoError(t, err, "%s case %d", name, i)
			require.Equal(t, c.want == nil, got.Value == nil, "%s case %d SQL NULL", name, i)
			var equal bool
			require.NoError(t, s.Scan(ctx, query, []any{got.Value, c.want}, &equal), "%s case %d", name, i)
			require.True(t, equal, "%s case %d: %v", name, i, got.Value)
			if sqlType == "numeric" {
				require.Equal(t, c.want, got.Value, "decimal scale")
			}
		}
		return nil
	}
}

type decimalText string

func (d decimalText) Value() (driver.Value, error) { return string(d), nil }

func TestPostgreSQLTypeInputs(t *testing.T) {
	db, pool := databases(t)
	instant := time.Date(2024, 2, 29, 23, 45, 6, 123456000, time.FixedZone("", -3*3600-1800))
	const uuid = "00000000-0000-4000-8000-000000000001"
	const decimal = "987654321012345678901234567890.010000"
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			cfg := q.RegistryConfig[struct{}]{}
			if driver == "pq" {
				cfg.ArrayArg = pqdriver.Array
			}
			reg := q.NewRegistry(cfg)
			f := typeFixture{t, db, typesSchema(t, db), q.DefineView(reg, q.ViewSpec[inputText]{Name: "entries"})}
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			checks := []callback{
				typeRoundTrip(f, "flag", "boolean", pg.Bool(), valueCase(true, "true")),
				typeRoundTrip(f, "small", "int2", pg.Int2(), valueCase(int16(-1234), "-1234")),
				typeRoundTrip(f, "integer", "int4", pg.Int4(), valueCase(int32(-123456), "-123456")),
				typeRoundTrip(f, "large", "int8", pg.Int8(), valueCase(int64(9876543210), "9876543210")),
				typeRoundTrip(f, "single", "float4", pg.Float4(), valueCase(float32(-1.25), "-1.25")),
				typeRoundTrip(f, "double", "float8", pg.Float8(), valueCase(1.0000000002, "1.0000000002")),
				typeRoundTrip(f, "identifier", "oid", pg.OID(), valueCase(uint32(4294967295), "4294967295")),
				typeRoundTrip(f, "textual", "text", pg.Text(), valueCase(" plain ", " plain ")),
				typeRoundTrip(f, "padded", "pg_catalog.bpchar", pg.BPChar(), valueCase("word  ", "word")),
				typeRoundTrip(f, "varying", "varchar", pg.Varchar(), valueCase("plain", "plain")),
				typeRoundTrip(f, "bounded", "varchar", pg.Varchar(2), valueCase("longer", "longer")),
				typeRoundTrip(f, "bytes", "bytea", pg.Bytea(), valueCase([]byte{0, 92, 255}, `\x005cff`),
					inputCase[[]byte]{}, valueCase([]byte{}, `\x`)),
				typeRoundTrip(f, "uuid", "uuid", pg.UUID[string](), valueCase(uuid, uuid)),
				typeRoundTrip(f, "decimal", "numeric", pg.Numeric[string](), valueCase(decimal, decimal)),
				typeRoundTrip(f, "valuer", "numeric", pg.Numeric[decimalText](), valueCase(decimalText(decimal), decimal)),
				typeRoundTrip(f, "document", "json", pg.JSON[any](), jsonInputCases()...),
				typeRoundTrip(f, "binary_document", "jsonb", pg.JSONB[any](), jsonInputCases()...),
				typeRoundTrip(f, "day", "date", pg.Date(), valueCase(instant, "2024-02-29")),
				typeRoundTrip(f, "civil", "timestamp", pg.Timestamp(), valueCase(instant, "2024-02-29 23:45:06.123456")),
				typeRoundTrip(f, "instant", "timestamptz", pg.Timestamptz(), valueCase(instant, "2024-03-01 03:15:06.123456+00")),
				typeRoundTrip(f, "clock", "time", pg.Time(), valueCase("24:00", "24:00:00")),
				typeRoundTrip(f, "offset_clock", "timetz", pg.Timetz(), valueCase("24:00+05:45", "24:00:00+05:45")),
				typeRoundTrip(f, "span", "interval", pg.Interval(),
					valueCase("1 year 2 mons 3 days 04:05:06.123456", "14 mons 3 days 04:05:06.123456")),
				typeRoundTrip(f, "host", "inet", pg.Inet(), valueCase(netip.MustParseAddr("192.0.2.1"), "192.0.2.1/32"),
					valueCase(netip.MustParseAddr("2001:db8::2"), "2001:db8::2/128"), inputCase[netip.Addr]{}),
				typeRoundTrip(f, "prefix", "inet", pg.InetPrefix(), valueCase(netip.MustParsePrefix("192.0.2.37/24"), "192.0.2.37/24"),
					valueCase(netip.MustParsePrefix("2001:db8::7/48"), "2001:db8::7/48"), inputCase[netip.Prefix]{}),
				typeRoundTrip(f, "network", "cidr", pg.CIDR(), valueCase(netip.MustParsePrefix("192.0.2.0/24"), "192.0.2.0/24"),
					valueCase(netip.MustParsePrefix("2001:db8::/48"), "2001:db8::/48"), inputCase[netip.Prefix]{}),
				typeRoundTrip(f, "members", "bigint[]", pg.ArrayOf(pg.Int8()), valueCase([]int64{3, 7}, "{3,7}"),
					valueCase([]int64{}, "{}"), inputCase[[]int64]{}),
			}
			for _, settings := range [][]string{
				{"SET LOCAL TIME ZONE 'UTC'", "SET LOCAL IntervalStyle='postgres'"},
				{"SET LOCAL TIME ZONE 'Asia/Kathmandu'", "SET LOCAL IntervalStyle='iso_8601'"},
			} {
				require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
					for _, setting := range settings {
						_, err := s.Exec(ctx, setting)
						require.NoError(t, err)
					}
					for _, check := range checks {
						require.NoError(t, check(ctx, s))
					}
					return nil
				}))
			}
		})
	}
}

func jsonInputCases() []inputCase[any] {
	return []inputCase[any]{
		valueCase[any]("plain", `"plain"`), valueCase[any](map[string]int{"x": 2}, `{"x":2}`),
		valueCase[any](json.RawMessage(`{"k":"v"}`), `{"k":"v"}`), valueCase[any](json.RawMessage("null"), "null"),
		valueCase[any]([]byte{0, 1, 2}, `"AAEC"`), {}, {value: json.RawMessage(nil)}, {value: []string(nil)},
		{value: map[string]int(nil)}, {value: (*string)(nil)},
	}
}
