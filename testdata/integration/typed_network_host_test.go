//go:build integration

package integration_test

import (
	"net/netip"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
)

func TestNetworkHost(t *testing.T) {
	scalarDrivers(t, func(f *scalarFixture) {
		addr := q.Input("address", pg.Inet())
		prefix := q.Input("prefix", pg.InetPrefix())
		for _, text := range []string{"192.0.2.7", "2001:db8::7"} {
			value := netip.MustParseAddr(text)
			scalarCheck(f, text, pg.Text(), q.Host(addr.Expr()), new(text), q.BindValue(addr, value))
		}
		for _, text := range []string{"192.0.2.7/24", "2001:db8::7/64"} {
			value := netip.MustParsePrefix(text)
			scalarCheck(f, text, pg.Text(), q.Host(prefix.Expr()), new(value.Addr().String()), q.BindValue(prefix, value))
		}
		scalarCheck(f, "null_address", pg.Text(), q.Host(q.TypedNull(pg.Inet())), nil)
		scalarCheck(f, "null_prefix", pg.Text(), q.Host(q.TypedNull(pg.InetPrefix())), nil)
	})
}
