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

func TestMultipleRegisteredLockProjectionQuery(t *testing.T) {
	const query = `SELECT "a".id, "b".id AS other FROM public.items AS "a" JOIN public.notes AS "b" ON "a".id="b".id`
	for _, suffix := range []string{
		` FOR UPDATE OF "a" FOR SHARE OF "b"`,
		` FOR NO KEY UPDATE OF "a" NOWAIT FOR KEY SHARE OF "b" SKIP LOCKED`,
		` FOR SHARE OF "a", "b"`,
	} {
		got, err := lockProjectionQuery(query + suffix)
		require.NoError(t, err)
		require.Equal(t, query, got)
	}
	_, err := lockProjectionQuery(`SELECT x.id FROM (SELECT id FROM public.items AS "i" FOR UPDATE OF "i") x`)
	require.ErrorIs(t, err, ErrDescribeRequired)
	for _, suffix := range []string{` FOR UPDATE OF "a" DELETE`, ` FOR UPDATE OF "a"; SELECT 2`, ` FOR UPDATE OF "a" FOR SHARE OF b`, ` FOR UPDATE OF "a" FOR UPDATE OF "b" OFFSET 1`} {
		_, err := lockProjectionQuery(query + suffix)
		require.Error(t, err)
	}
}
