package postgresfixture

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoginRoleOptions(t *testing.T) {
	attrs, err := (LoginRoleOptions{}).attributes()
	require.NoError(t, err)
	require.Equal(t, "LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT", attrs)
	attrs, err = (LoginRoleOptions{MemberOf: []string{`Mixed"Case`, "a,b"}, Inherit: true, ConnectionLimit: new(0)}).attributes()
	require.NoError(t, err)
	require.Contains(t, attrs, `INHERIT CONNECTION LIMIT 0 IN ROLE "Mixed""Case", "a,b"`)
	for _, opts := range []LoginRoleOptions{
		{ConnectionLimit: new(-2)}, {MemberOf: []string{"r", "r"}}, {MemberOf: []string{""}},
		{MemberOf: []string{strings.Repeat("a", 64)}}, {MemberOf: []string{"a\x00b"}}, {MemberOf: []string{"\xff"}},
	} {
		_, err = opts.attributes()
		require.Error(t, err)
	}
	var f Fixture
	_, err = f.AcquireLoginRole(t.Context(), LoginRoleOptions{})
	require.Error(t, err)
	_, err = f.PruneLoginRoles(t.Context(), 0)
	require.Error(t, err)
	_, err = f.PruneLoginRoles(t.Context(), -time.Second)
	require.ErrorContains(t, err, "nonnegative")
	var zero LoginRole
	require.Empty(t, zero.Name())
	require.Empty(t, zero.Password())
	require.Error(t, zero.Release(context.Background()))
}

func TestLoginIdleGrace(t *testing.T) {
	const base = "tabula fixture login v1 7/9"
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	eligible, mark, err := loginIdle(base, false, now, time.Hour)
	require.NoError(t, err)
	require.False(t, eligible)
	require.Equal(t, base+" idle 2026-01-02T03:04:05Z", mark)
	for _, tc := range []struct {
		elapsed time.Duration
		want    bool
	}{{0, false}, {time.Hour - time.Nanosecond, false}, {time.Hour, true}} {
		eligible, update, err := loginIdle(mark, false, now.Add(tc.elapsed), time.Hour)
		require.NoError(t, err)
		require.Empty(t, update)
		require.Equal(t, tc.want, eligible)
	}
	_, _, err = loginIdle(base+" idle malformed", false, now, 0)
	require.Error(t, err)
}
