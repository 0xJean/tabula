//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/plan"
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
		i, big, text := plan.Input("i", pg.Int4()), plan.Input("big", pg.Int8()), plan.Input("text", pg.Text())
		address := plan.Input("address", pg.Inet())
		day, clock := plan.Input("day", pg.Date()), plan.Input("clock", pg.Time())
		for _, c := range []struct {
			fn   string
			expr plan.Expr[string]
			args []plan.Binding
		}{
			{"pg_catalog.int4pl(int4,int4)", plan.Cast(pg.Text(), plan.Plus(i.Expr(), i.Expr())), []plan.Binding{plan.BindValue(i, int32(1))}},
			{"pg_catalog.int4(int8)", plan.Cast(pg.Text(), plan.Cast(pg.Int4(), big.Expr())), []plan.Binding{plan.BindValue(big, int64(1))}},
			{"pg_catalog.uuid_in(cstring)", plan.Cast(pg.Text(), plan.Cast(pg.UUID[string](), text.Expr())), []plan.Binding{plan.BindValue(text, "00000000-0000-4000-8000-000000000001")}},
			{"pg_catalog.lower(text)", plan.Lower(text.Expr()), []plan.Binding{plan.BindValue(text, "ABC")}},
			{"pg_catalog.array_remove(anycompatiblearray,anycompatible)", plan.Cast(pg.Text(), plan.NullIf(i.Expr(), i.Expr())), []plan.Binding{plan.BindValue(i, int32(1))}},
			{"pg_catalog.gen_random_uuid()", plan.Cast(pg.Text(), plan.GenRandomUUID()), nil},
			{"pg_catalog.count()", plan.Cast(pg.Text(), plan.CountAll()), nil},
			{"pg_catalog.host(inet)", plan.Host(address.Expr()), []plan.Binding{plan.BindValue(address, netip.MustParseAddr("192.0.2.7"))}},
			{"pg_catalog.hashtextextended(text,int8)", plan.Cast(pg.Text(), plan.HashTextExtended(text.Expr(), big.Expr())), []plan.Binding{plan.BindValue(text, "items"), plan.BindValue(big, int64(-7))}},
			{"pg_catalog.datetime_pl(date,time)", plan.Cast(pg.Text(), plan.PlusAs(pg.Timestamp(), day.Expr(), clock.Expr())), []plan.Binding{plan.BindValue(day, time.Date(2020, 2, 3, 0, 0, 0, 0, time.UTC)), plan.BindValue(clock, "00:00:00")}},
			{"pg_catalog.timedate_pl(time,date)", plan.Cast(pg.Text(), plan.PlusAs(pg.Timestamp(), clock.Expr(), day.Expr())), []plan.Binding{plan.BindValue(day, time.Date(2020, 2, 3, 0, 0, 0, 0, time.UTC)), plan.BindValue(clock, "00:00:00")}},
			{"pg_catalog.row_number()", plan.Cast(pg.Text(), plan.Over(plan.RowNumber(), plan.WindowSpec{})), nil},
			{"pg_catalog.rank()", plan.Cast(pg.Text(), plan.Over(plan.Rank(), plan.WindowSpec{})), nil},
			{"pg_catalog.dense_rank()", plan.Cast(pg.Text(), plan.Over(plan.DenseRank(), plan.WindowSpec{})), nil},
			{"pg_catalog.percent_rank()", plan.Cast(pg.Text(), plan.Over(plan.PercentRank(), plan.WindowSpec{})), nil},
			{"pg_catalog.cume_dist()", plan.Cast(pg.Text(), plan.Over(plan.CumeDist(), plan.WindowSpec{})), nil},
			{"pg_catalog.ntile(integer)", plan.Cast(pg.Text(), plan.Over(plan.NTile(i.Expr()), plan.WindowSpec{})), []plan.Binding{plan.BindValue(i, int32(2))}},
			{"pg_catalog.lag(anycompatible,integer,anycompatible)", plan.Cast(pg.Text(), plan.Over(plan.Lag(i.Expr(), i.Expr(), i.Expr()), plan.WindowSpec{})), []plan.Binding{plan.BindValue(i, int32(1))}},
			{"pg_catalog.lead(anycompatible,integer,anycompatible)", plan.Cast(pg.Text(), plan.Over(plan.Lead(i.Expr(), i.Expr(), i.Expr()), plan.WindowSpec{})), []plan.Binding{plan.BindValue(i, int32(1))}},
			{"pg_catalog.first_value(anyelement)", plan.Cast(pg.Text(), plan.Over(plan.FirstValue(i.Expr()), plan.WindowSpec{})), []plan.Binding{plan.BindValue(i, int32(1))}},
			{"pg_catalog.last_value(anyelement)", plan.Cast(pg.Text(), plan.Over(plan.LastValue(i.Expr()), plan.WindowSpec{})), []plan.Binding{plan.BindValue(i, int32(1))}},
			{"pg_catalog.nth_value(anyelement,integer)", plan.Cast(pg.Text(), plan.Over(plan.NthValue(i.Expr(), i.Expr()), plan.WindowSpec{})), []plan.Binding{plan.BindValue(i, int32(1))}},
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
		p := scalarPlan(f, pg.Int4(), plan.NullIf(i.Expr(), plan.TypedNull(pg.Int4())))
		exec("REVOKE EXECUTE ON FUNCTION pg_catalog.int4eq(int4,int4) FROM PUBLIC")
		err := run(t.Context(), func(ctx context.Context, s session) error {
			_, err := p.Bind(s).Find(ctx, plan.BindValue(i, int32(3)))
			return err
		})
		exec("GRANT EXECUTE ON FUNCTION pg_catalog.int4eq(int4,int4) TO PUBLIC")
		require.NoError(t, err)
	})
}
