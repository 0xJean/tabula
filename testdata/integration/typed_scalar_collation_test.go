//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/stretchr/testify/require"
)

func TestScalarTextCollation(t *testing.T) {
	type item struct {
		ID    int64  `db:"id,key"`
		Label string `db:"label"`
	}
	db, pool := databases(t)
	var schema, name, provider string
	err := db.QueryRowContext(t.Context(), `SELECT n.nspname,c.collname,c.collprovider::text
FROM pg_collation c JOIN pg_namespace n ON n.oid=c.collnamespace
WHERE c.collname NOT IN ('default','C','POSIX','ucs_basic')
AND c.collencoding IN (-1,pg_char_to_encoding(current_setting('server_encoding')))
AND c.collisdeterministic
ORDER BY (c.collname='tr-x-icu') DESC,c.oid LIMIT 1`).Scan(&schema, &name, &provider)
	require.NoError(t, err, "the disposable fixture needs an available deterministic non-C collation")
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	locale := quote(schema) + "." + quote(name)
	t.Logf("non-C collation: %s, provider %s", locale, provider)
	for _, collation := range []string{`"pg_catalog"."C"`, locale} {
		tableName := typesSchema(t, db) + ".items"
		_, err := db.ExecContext(t.Context(), "CREATE TABLE "+tableName+" (id bigint PRIMARY KEY,label text COLLATE "+collation+" NOT NULL)")
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), "INSERT INTO "+tableName+" VALUES (1,$1)", "IÉ")
		require.NoError(t, err)
		var want string
		require.NoError(t, db.QueryRowContext(t.Context(), "SELECT pg_catalog.lower(label) FROM "+tableName).Scan(&want))
		for _, driver := range []string{"pq", "pgx"} {
			t.Run(driver+"/"+collation, func(t *testing.T) {
				reg := q.NewRegistry(q.RegistryConfig[struct{}]{})
				table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: tableName, RelationScope: q.Global()})
				src := q.MapTable(table).As("i")
				view := q.DefineView(reg, q.ViewSpec[scalarValue[string]]{Name: "entries"})
				label := q.FieldOf(src.Rel(), "label", pg.Text())
				plan := q.DefineRead(view, "lower", q.Select(q.Project(q.Output(view, "value", pg.Text()), q.Lower(label.Expr()))).From(src))
				run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
				require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
					rows, err := plan.Bind(s).Find(ctx)
					require.NoError(t, err)
					require.Len(t, rows, 1)
					require.Equal(t, new(want), rows[0].Value)
					return nil
				}))
			})
		}
	}
}
