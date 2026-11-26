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
	_, err = f.PruneClones(t.Context(), 0)
	require.ErrorContains(t, err, "Admin")
	_, err = f.PruneClones(t.Context(), -time.Second)
	require.ErrorContains(t, err, "nonnegative")
	var zero LoginRole
	for _, role := range []*LoginRole{nil, &zero} {
		_, err = f.CloneWithLease(t.Context(), Installation{}, role)
		require.ErrorContains(t, err, "requires a fixture login")
	}
	require.Empty(t, zero.Name())
	require.Empty(t, zero.Password())
	require.Error(t, zero.Release(context.Background()))
}

func TestLoginLabel(t *testing.T) {
	maxLabel := strings.Repeat("a", 25)
	for _, label := range []string{"", "_", "worktree_2", maxLabel} {
		name, err := freshLoginName(label)
		require.NoError(t, err)
		require.Regexp(t, loginNamePattern, name)
		if label == "" {
			require.Len(t, name, 37)
		} else {
			require.True(t, strings.HasSuffix(name, "_"+label))
			require.Len(t, name, 38+len(label))
		}
	}
	for _, label := range []string{"Topic", "a/b", "a-b", "x y", "x\n", "é", "x'_", maxLabel + "0"} {
		_, err := freshLoginName(label)
		require.Error(t, err)
	}
}

func TestFixtureIdleGrace(t *testing.T) {
	const base = "tabula fixture login v1 7/9"
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	eligible, mark, err := fixtureIdle(base, false, now, time.Hour)
	require.NoError(t, err)
	require.False(t, eligible)
	require.Equal(t, base+" idle 2026-01-02T03:04:05Z", mark)
	for _, tc := range []struct {
		elapsed time.Duration
		want    bool
	}{{0, false}, {time.Hour - time.Nanosecond, false}, {time.Hour, true}} {
		eligible, update, err := fixtureIdle(mark, false, now.Add(tc.elapsed), time.Hour)
		require.NoError(t, err)
		require.Empty(t, update)
		require.Equal(t, tc.want, eligible)
	}
	_, _, err = fixtureIdle(base+" idle malformed", false, now, 0)
	require.Error(t, err)
}
