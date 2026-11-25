package postgresfixture

import (
	"context"
	"strings"
	"testing"

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
	_, err = f.PruneLoginRoles(t.Context())
	require.Error(t, err)
	var zero LoginRole
	require.Empty(t, zero.Name())
	require.Empty(t, zero.Password())
	require.Error(t, zero.Release(context.Background()))
}
