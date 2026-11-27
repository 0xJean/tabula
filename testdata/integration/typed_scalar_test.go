//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/plan"
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
	src    plan.RowSource[scalarItem, struct{}]
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
			f := &scalarFixture{t: t, db: db, pool: pool, reg: reg, src: plan.MapTable(table).As("i"), driver: driver}
			f.run = authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			test(f)
		})
	}
}

func scalarPlan[V any](f *scalarFixture, kind q.PGType[V], value plan.Expr[V]) *plan.ReadPlan[scalarValue[V], struct{}] {
	t := f.t
	t.Helper()
	f.serial++
	view := q.DefineView(f.reg, q.ViewSpec[scalarValue[V]]{Name: fmt.Sprintf("entries_%d", f.serial)})
	p := plan.DefineRead(view, "value", plan.Select(plan.Project(plan.Output(view, "value", kind), value)).From(f.src))
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

func scalarCheck[V any](f *scalarFixture, name string, kind q.PGType[V], value plan.Expr[V], want *V, bindings ...plan.Binding) {
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
		s, other := plan.Input("text", pg.Text()), plan.Input("other", pg.Text())
		start, count := plan.Input("start", pg.Int4()), plan.Input("count", pg.Int4())
		pattern, replacement, flags := plan.Input("pattern", pg.Text()), plan.Input("replacement", pg.Text()), plan.Input("flags", pg.Text())
		bytes := plan.Input("bytes", pg.Bytea())
		enc := plan.Input("encoding", pg.Text())
		scalarCheck(f, "lower", pg.Text(), plan.Lower(s.Expr()), new(" abc "), plan.BindValue(s, " ABC "))
		scalarCheck(f, "upper", pg.Text(), plan.Upper(s.Expr()), new("ABC"), plan.BindValue(s, "abc"))
		scalarCheck(f, "trim", pg.Text(), plan.Trim(s.Expr()), new("abc"), plan.BindValue(s, " abc "))
		scalarCheck(f, "btrim", pg.Text(), plan.BTrim(s.Expr(), other.Expr()), new("abc"), plan.BindValue(s, "xyabcxy"), plan.BindValue(other, "xy"))
		scalarCheck(f, "length", pg.Int4(), plan.Length(s.Expr()), new(int32(3)), plan.BindValue(s, "aé\u754c"))
		scalarCheck(f, "substr_rest", pg.Text(), plan.Substr(s.Expr(), start.Expr()), new("é\u754c"), plan.BindValue(s, "aé\u754c"), plan.BindValue(start, int32(2)))
		scalarCheck(f, "substr_count", pg.Text(), plan.Substr(s.Expr(), start.Expr(), count.Expr()), new("é"), plan.BindValue(s, "aé\u754c"), plan.BindValue(start, int32(2)), plan.BindValue(count, int32(1)))
		scalarCheck(f, "prefix", pg.Bool(), plan.StartsWith(s.Expr(), other.Expr()), new(true), plan.BindValue(s, "éclair"), plan.BindValue(other, "é"))
		scalarCheck(f, "replace", pg.Text(), plan.RegexpReplace(s.Expr(), pattern.Expr(), replacement.Expr(), flags.Expr()), new("a-a"), plan.BindValue(s, "A-A"), plan.BindValue(pattern, "A"), plan.BindValue(replacement, "a"), plan.BindValue(flags, "g"))
		for _, v := range []struct {
			name string
			expr plan.Pred
			want bool
		}{
			{"regex", plan.RegexpMatch(s.Expr(), pattern.Expr()), false}, {"iregex", plan.RegexpIMatch(s.Expr(), pattern.Expr()), true},
			{"not_regex", plan.RegexpNotMatch(s.Expr(), pattern.Expr()), true}, {"not_iregex", plan.RegexpNotIMatch(s.Expr(), pattern.Expr()), false},
		} {
			scalarCheck(f, v.name, pg.Bool(), v.expr, new(v.want), plan.BindValue(s, "ABC"), plan.BindValue(pattern, "^a"))
		}
		scalarCheck(f, "concat", pg.Text(), plan.Concat(plan.TypedNull(pg.Text()), s.Expr(), start.Expr()), new("v3"), plan.BindValue(s, "v"), plan.BindValue(start, int32(3)))
		scalarCheck(f, "strict_cat", pg.Text(), plan.TextCat(plan.TypedNull(pg.Text()), s.Expr()), nil, plan.BindValue(s, "v"))
		scalarCheck(f, "byte_cat", pg.Bytea(), plan.ByteaCat(bytes.Expr(), bytes.Expr()), new([]byte{0, 255, 0, 255}), plan.BindValue(bytes, []byte{0, 255}))
		scalarCheck(f, "hex", pg.Text(), plan.Encode(bytes.Expr(), plan.Hex), new("005cff"), plan.BindValue(bytes, []byte{0, 92, 255}))
		scalarCheck(f, "base64", pg.Text(), plan.Encode(bytes.Expr(), plan.Base64), new("AFz/"), plan.BindValue(bytes, []byte{0, 92, 255}))
		scalarCheck(f, "escape", pg.Text(), plan.Encode(bytes.Expr(), plan.Escape), new(`\000\\\377`), plan.BindValue(bytes, []byte{0, 92, 255}))
		scalarCheck(f, "sha256", pg.Text(), plan.Encode(plan.SHA256(plan.ConvertTo(s.Expr(), enc.Expr())), plan.Hex), new("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"), plan.BindValue(s, "abc"), plan.BindValue(enc, "UTF8"))
		scalarCheck(f, "empty_bytes", pg.Text(), plan.Encode(bytes.Expr(), plan.Hex), new(""), plan.BindValue(bytes, []byte{}))
		scalarCheck(f, "null_bytes", pg.Text(), plan.Encode(bytes.Expr(), plan.Hex), nil, plan.BindValue(bytes, []byte(nil)))
	})
}
