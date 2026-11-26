//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

func TestScalarNativePrivileges(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		t := f.t
		role := fmt.Sprintf("scope_reader_%d", time.Now().UnixNano())
		exec := func(sql string) { t.Helper(); _, err := f.db.ExecContext(t.Context(), sql); require.NoError(t, err) }
		exec("CREATE ROLE " + role)
		t.Cleanup(func() { _, err := f.db.Exec("DROP OWNED BY " + role + "; DROP ROLE " + role); require.NoError(t, err) })
		for _, d := range f.reg.Defined() {
			exec("GRANT SELECT(id) ON " + d.Name + " TO " + role)
			// The fixture creates schema-qualified names from package-owned labels.
			var schema string
			require.NoError(t, f.db.QueryRowContext(t.Context(), "SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.oid=$1::regclass", d.Name).Scan(&schema))
			exec("GRANT USAGE ON SCHEMA " + schema + " TO " + role)
		}
		run := authorizedEffectRunner(t, f.driver, f.db, f.pool, role, f.reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
		i, big, text := q.Input("i", pg.Int4()), q.Input("big", pg.Int8()), q.Input("text", pg.Text())
		for _, c := range []struct {
			fn   string
			expr q.Expr[string]
			args []q.Binding
		}{
			{"pg_catalog.int4pl(int4,int4)", q.Cast(pg.Text(), q.Plus(i.Expr(), i.Expr())), []q.Binding{q.BindValue(i, int32(1))}},
			{"pg_catalog.int4(int8)", q.Cast(pg.Text(), q.Cast(pg.Int4(), big.Expr())), []q.Binding{q.BindValue(big, int64(1))}},
			{"pg_catalog.uuid_in(cstring)", q.Cast(pg.Text(), q.Cast(pg.UUID[string](), text.Expr())), []q.Binding{q.BindValue(text, "00000000-0000-4000-8000-000000000001")}},
			{"pg_catalog.lower(text)", q.Lower(text.Expr()), []q.Binding{q.BindValue(text, "ABC")}},
			{"pg_catalog.array_remove(anycompatiblearray,anycompatible)", q.Cast(pg.Text(), q.NullIf(i.Expr(), i.Expr())), []q.Binding{q.BindValue(i, int32(1))}},
			{"pg_catalog.gen_random_uuid()", q.Cast(pg.Text(), q.GenRandomUUID()), nil},
			{"pg_catalog.count()", q.Cast(pg.Text(), q.CountAll()), nil},
		} {
			p := scalarPlan(f, pg.Text(), c.expr)
			execute := func() error {
				return run(t.Context(), func(ctx context.Context, s session) error { _, err := p.Bind(s).Find(ctx, c.args...); return err })
			}
			require.NoError(t, execute())
			exec("REVOKE EXECUTE ON FUNCTION " + c.fn + " FROM PUBLIC")
			err := execute()
			// Readiness inspects catalog privileges in the same database under
			// the same role. Restore PUBLIC before checking either result.
			work := sqlcheck.RoleStatements{}
			work.AddViews(role, f.reg.DefinedViews()...)
			needs, needsErr := work.Readiness(role)
			missing, checkErr := sqlcheck.CheckReadiness(t.Context(), f.db, role, needs)
			exec("GRANT EXECUTE ON FUNCTION " + c.fn + " TO PUBLIC")
			require.NoError(t, needsErr)
			require.NoError(t, checkErr)
			require.Len(t, missing, 1, c.fn)
			require.Equal(t, "FUNCTION", missing[0].Kind)
			require.Equal(t, "EXECUTE", missing[0].Privilege)
			var sameFunction bool
			require.NoError(t, f.db.QueryRowContext(t.Context(), "SELECT $1::regprocedure = $2::regprocedure", c.fn, missing[0].Object).Scan(&sameFunction))
			require.True(t, sameFunction, c.fn)
			require.Equal(t, "42501", q.SQLState(err), c.fn)
		}
		// NULLIF scalar equality is internal to array_remove: no caller eq grant.
		p := scalarPlan(f, pg.Int4(), q.NullIf(i.Expr(), q.TypedNull(pg.Int4())))
		exec("REVOKE EXECUTE ON FUNCTION pg_catalog.int4eq(int4,int4) FROM PUBLIC")
		err := run(t.Context(), func(ctx context.Context, s session) error {
			_, err := p.Bind(s).Find(ctx, q.BindValue(i, int32(3)))
			return err
		})
		exec("GRANT EXECUTE ON FUNCTION pg_catalog.int4eq(int4,int4) TO PUBLIC")
		require.NoError(t, err)
	})
}
