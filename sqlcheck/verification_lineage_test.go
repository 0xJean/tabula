package sqlcheck

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExplicitResultCast(t *testing.T) {
	for _, test := range []struct {
		sql   string
		index int
		want  string
	}{
		{`SELECT CAST($1 AS "public"."kind") AS "note"`, 0, `"public"."kind"`},
		{`WITH x AS (SELECT 1) SELECT CAST((SELECT 2) AS pg_catalog.int4) AS id`, 0, `pg_catalog.int4`},
		{`INSERT INTO public.items(id) VALUES($1) RETURNING id, CAST(note AS public.kind) AS note`, 1, `public.kind`},
		{`SELECT DISTINCT ON (id) CAST(note AS public.kind) AS note FROM public.items`, 0, `public.kind`},
		{`SELECT CASE WHEN true THEN CAST($1 AS public.kind) ELSE $2 END AS note`, 0, ``},
		{`SELECT 'CAST($1 AS public.kind)' AS note`, 0, ``},
		{`SELECT (SELECT CAST($1 AS public.kind)) AS note`, 0, ``},
		{`SELECT CAST($1 AS public.kind)::text AS note`, 0, ``},
		{`SELECT CAST($1 AS public.kind) || '' AS note`, 0, ``},
		{`WITH x AS (SELECT CAST($1 AS public.kind) AS note) SELECT note FROM x`, 0, ``},
		{`SELECT CAST($1 AS varchar(4)) AS note`, 0, ``},
		{`SELECT CAST($1 AS public.kind) AS note UNION ALL SELECT $2::text AS note`, 0, ``},
		{`WITH x AS (SELECT 1) SELECT CAST($1 AS public.kind) AS note EXCEPT SELECT $2::text`, 0, ``},
		{`SELECT i.*, CAST($1 AS public.kind) AS note FROM public.items i`, 1, ``},
		{`SELECT CAST($1 AS public.kind) AS note; SELECT 1`, 0, ``},
	} {
		got, ok := explicitResultCast(test.sql, test.index)
		require.Equal(t, test.want != "", ok, test.sql)
		require.Equal(t, test.want, got, test.sql)
	}
}
