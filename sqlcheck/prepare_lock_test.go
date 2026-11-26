package sqlcheck

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegisteredLockProjectionQuery(t *testing.T) {
	const query = `SELECT "FOR".id AS "label FOR UPDATE OF ""i""" FROM "public"."items FOR SHARE" AS "FOR"`
	for _, strength := range []string{"UPDATE", "NO KEY UPDATE", "SHARE", "KEY SHARE"} {
		for _, wait := range []string{"", " SKIP LOCKED", " NOWAIT"} {
			got, err := lockProjectionQuery(query + " FOR " + strength + ` OF "FOR"` + wait)
			require.NoError(t, err)
			require.Equal(t, query, got)
		}
	}
	for _, suffix := range []string{"", " FOR UPDATE", " FOR UPDATE OF i", ` FOR UPDATE OF "FOR" LIMIT 1`, ` FOR UPDATE OF "FOR" SKIP`} {
		_, err := lockProjectionQuery(query + suffix)
		require.ErrorContains(t, err, "expected a registered trailing row-lock clause")
	}
}
