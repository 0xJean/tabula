//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

type scalarItem struct {
	ID int64 `db:"id,key"`
}
type scalarValue[V any] struct {
	Value *V `db:"value"`
}

type scalarFixture struct {
	t      *testing.T
	db     *sql.DB
	pool   *pgxpool.Pool
	reg    *q.Registry[struct{}]
	src    q.RowSource[scalarItem, struct{}]
	run    effectRunner
	serial int
	driver string
}

func scalarDrivers(t *testing.T, test func(*scalarFixture)) {
	t.Helper()
	db, pool := databases(t)
	schema := typesSchema(t, db)
	_, err := db.ExecContext(t.Context(), "CREATE TABLE "+schema+".items (id bigint PRIMARY KEY); INSERT INTO "+schema+".items VALUES (1)")
	require.NoError(t, err)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			cfg := q.RegistryConfig[struct{}]{}
			if driver == "pq" {
				cfg.ArrayArg = pqdriver.Array
			}
			reg := q.NewRegistry(cfg)
			table := q.DefinePlanSource(reg, q.TableSpec[scalarItem, int64]{Name: schema + ".items", RelationScope: q.Global()})
			f := &scalarFixture{t: t, db: db, pool: pool, reg: reg, src: q.MapTable(table).As("i"), driver: driver}
			f.run = authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			test(f)
		})
	}
}

func scalarPlan[V any](f *scalarFixture, kind q.PGType[V], value q.Expr[V]) *q.ReadPlan[scalarValue[V], struct{}] {
	t := f.t
	t.Helper()
	f.serial++
	view := q.DefineView(f.reg, q.ViewSpec[scalarValue[V]]{Name: fmt.Sprintf("entries_%d", f.serial)})
	p := q.DefineRead(view, "value", q.Select(q.Project(q.Output(view, "value", kind), value)).From(f.src))
	def := view.Definition()
	conn, err := f.pool.Acquire(t.Context())
	require.NoError(t, err)
	defer conn.Release()
	for key, query := range def.Statements {
		desc, err := conn.Conn().Prepare(t.Context(), "", query)
		require.NoError(t, err)
		shape := def.StatementShapes[key]
		require.Len(t, desc.Fields, 1)
		var oid uint32
		require.NoError(t, f.db.QueryRowContext(t.Context(), "SELECT $1::regtype::oid", shape.Result[0].Type).Scan(&oid))
		require.Equal(t, oid, desc.Fields[0].DataTypeOID, "result OID")
		require.Len(t, desc.ParamOIDs, len(shape.Inputs))
		for i, input := range shape.Inputs {
			require.NoError(t, f.db.QueryRowContext(t.Context(), "SELECT $1::regtype::oid", input.Type).Scan(&oid))
			require.Equal(t, oid, desc.ParamOIDs[i], "parameter %s", input.Name)
		}
		for _, grant := range shape.Privileges {
			if grant.Privilege != "EXECUTE" {
				continue
			}
			var exists bool
			require.NoError(t, f.db.QueryRowContext(t.Context(), "SELECT to_regprocedure($1) IS NOT NULL", grant.Function).Scan(&exists))
			require.True(t, exists, "native signature %s", grant.Function)
		}
	}
	return p
}

func scalarCheck[V any](f *scalarFixture, name string, kind q.PGType[V], value q.Expr[V], want *V, bindings ...q.Binding) {
	f.t.Helper()
	f.t.Run(name, func(t *testing.T) {
		parent := f.t
		f.t = t
		defer func() { f.t = parent }()
		p := scalarPlan(f, kind, value)
		require.NoError(t, f.run(t.Context(), func(ctx context.Context, s session) error {
			got, err := p.Bind(s).Find(ctx, bindings...)
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, want, got[0].Value)
			return nil
		}))
	})
}

func TestScalarTextAndBytes(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		s, other := q.Input("text", pg.Text()), q.Input("other", pg.Text())
		start, count := q.Input("start", pg.Int4()), q.Input("count", pg.Int4())
		pattern, replacement, flags := q.Input("pattern", pg.Text()), q.Input("replacement", pg.Text()), q.Input("flags", pg.Text())
		bytes := q.Input("bytes", pg.Bytea())
		enc := q.Input("encoding", pg.Text())
		scalarCheck(f, "lower", pg.Text(), q.Lower(s.Expr()), new(" abc "), q.BindValue(s, " ABC "))
		scalarCheck(f, "upper", pg.Text(), q.Upper(s.Expr()), new("ABC"), q.BindValue(s, "abc"))
		scalarCheck(f, "trim", pg.Text(), q.Trim(s.Expr()), new("abc"), q.BindValue(s, " abc "))
		scalarCheck(f, "btrim", pg.Text(), q.BTrim(s.Expr(), other.Expr()), new("abc"), q.BindValue(s, "xyabcxy"), q.BindValue(other, "xy"))
		scalarCheck(f, "length", pg.Int4(), q.Length(s.Expr()), new(int32(3)), q.BindValue(s, "aé\u754c"))
		scalarCheck(f, "substr_rest", pg.Text(), q.Substr(s.Expr(), start.Expr()), new("é\u754c"), q.BindValue(s, "aé\u754c"), q.BindValue(start, int32(2)))
		scalarCheck(f, "substr_count", pg.Text(), q.Substr(s.Expr(), start.Expr(), count.Expr()), new("é"), q.BindValue(s, "aé\u754c"), q.BindValue(start, int32(2)), q.BindValue(count, int32(1)))
		scalarCheck(f, "prefix", pg.Bool(), q.StartsWith(s.Expr(), other.Expr()), new(true), q.BindValue(s, "éclair"), q.BindValue(other, "é"))
		scalarCheck(f, "replace", pg.Text(), q.RegexpReplace(s.Expr(), pattern.Expr(), replacement.Expr(), flags.Expr()), new("a-a"), q.BindValue(s, "A-A"), q.BindValue(pattern, "A"), q.BindValue(replacement, "a"), q.BindValue(flags, "g"))
		for _, v := range []struct {
			name string
			expr q.Pred
			want bool
		}{
			{"regex", q.RegexpMatch(s.Expr(), pattern.Expr()), false}, {"iregex", q.RegexpIMatch(s.Expr(), pattern.Expr()), true},
			{"not_regex", q.RegexpNotMatch(s.Expr(), pattern.Expr()), true}, {"not_iregex", q.RegexpNotIMatch(s.Expr(), pattern.Expr()), false},
		} {
			scalarCheck(f, v.name, pg.Bool(), v.expr, new(v.want), q.BindValue(s, "ABC"), q.BindValue(pattern, "^a"))
		}
		scalarCheck(f, "concat", pg.Text(), q.Concat(q.TypedNull(pg.Text()), s.Expr(), start.Expr()), new("v3"), q.BindValue(s, "v"), q.BindValue(start, int32(3)))
		scalarCheck(f, "strict_cat", pg.Text(), q.TextCat(q.TypedNull(pg.Text()), s.Expr()), nil, q.BindValue(s, "v"))
		scalarCheck(f, "byte_cat", pg.Bytea(), q.ByteaCat(bytes.Expr(), bytes.Expr()), new([]byte{0, 255, 0, 255}), q.BindValue(bytes, []byte{0, 255}))
		scalarCheck(f, "hex", pg.Text(), q.Encode(bytes.Expr(), q.Hex), new("005cff"), q.BindValue(bytes, []byte{0, 92, 255}))
		scalarCheck(f, "base64", pg.Text(), q.Encode(bytes.Expr(), q.Base64), new("AFz/"), q.BindValue(bytes, []byte{0, 92, 255}))
		scalarCheck(f, "escape", pg.Text(), q.Encode(bytes.Expr(), q.Escape), new(`\000\\\377`), q.BindValue(bytes, []byte{0, 92, 255}))
		scalarCheck(f, "sha256", pg.Text(), q.Encode(q.SHA256(q.ConvertTo(s.Expr(), enc.Expr())), q.Hex), new("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"), q.BindValue(s, "abc"), q.BindValue(enc, "UTF8"))
		scalarCheck(f, "empty_bytes", pg.Text(), q.Encode(bytes.Expr(), q.Hex), new(""), q.BindValue(bytes, []byte{}))
		scalarCheck(f, "null_bytes", pg.Text(), q.Encode(bytes.Expr(), q.Hex), nil, q.BindValue(bytes, []byte(nil)))
	})
}
