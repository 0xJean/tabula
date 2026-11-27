//go:build integration

package integration_test

import (
	"net/netip"
	"testing"

	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/plan"
)

func TestNetworkHost(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		addr := plan.Input("address", pg.Inet())
		prefix := plan.Input("prefix", pg.InetPrefix())
		for _, text := range []string{"192.0.2.7", "2001:db8::7"} {
			value := netip.MustParseAddr(text)
			scalarCheck(f, text, pg.Text(), plan.Host(addr.Expr()), new(text), plan.BindValue(addr, value))
		}
		for _, text := range []string{"192.0.2.7/24", "2001:db8::7/64"} {
			value := netip.MustParsePrefix(text)
			scalarCheck(f, text, pg.Text(), plan.Host(prefix.Expr()), new(value.Addr().String()), plan.BindValue(prefix, value))
		}
		scalarCheck(f, "null_address", pg.Text(), plan.Host(plan.TypedNull(pg.Inet())), nil)
		scalarCheck(f, "null_prefix", pg.Text(), plan.Host(plan.TypedNull(pg.InetPrefix())), nil)
	})
}
