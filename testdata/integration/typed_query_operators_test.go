//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/stdsql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func TestQueryHiddenComparisonsIgnoreSearchPathDrivers(t *testing.T) {
	type item struct {
		ID      int64     `db:"id,key"`
		Scope   itemScope `db:"scope_id,scope"`
		Deleted bool      `db:"deleted,deleted"`
	}
	type note struct {
		ID    int64  `db:"id,key"`
		State string `db:"state,deleted"`
	}
	db, _ := databases(t)
	schema := typesSchema(t, db)
	_, err := db.ExecContext(t.Context(), "CREATE TABLE "+schema+".items(id bigint PRIMARY KEY,scope_id bigint NOT NULL,deleted boolean NOT NULL); INSERT INTO "+schema+".items VALUES(1,1,false),(2,2,false),(3,1,true); CREATE FUNCTION "+schema+".int8_equal(bigint,bigint) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT true'; CREATE OPERATOR "+schema+".= (LEFTARG=bigint, RIGHTARG=bigint, FUNCTION="+schema+".int8_equal); CREATE FUNCTION "+schema+".bool_equal(boolean,boolean) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT true'; CREATE OPERATOR "+schema+".= (LEFTARG=boolean, RIGHTARG=boolean, FUNCTION="+schema+".bool_equal)")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "CREATE TABLE "+schema+".notes(id bigint PRIMARY KEY,state text NOT NULL); INSERT INTO "+schema+".notes VALUES(1,'open'),(2,'closed'); CREATE FUNCTION "+schema+".text_ne(text,text) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT true'; CREATE OPERATOR "+schema+".<> (LEFTARG=text, RIGHTARG=text, FUNCTION="+schema+".text_ne)")
	require.NoError(t, err)
	dsn := os.Getenv("TABULA_TEST_DSN")
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		connection, err := url.Parse(dsn)
		require.NoError(t, err)
		params := connection.Query()
		params.Set("search_path", schema+",pg_catalog")
		connection.RawQuery = params.Encode()
		dsn = connection.String()
	} else {
		dsn += " search_path='" + schema + ",pg_catalog'"
	}
	readDB, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = readDB.Close() })
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			reg := q.NewRegistry(q.RegistryConfig[itemScope]{Deny: q.DenyLegacySQL, DefaultTypes: []q.TypeDefault{q.TypeMapping(q.CatalogType[itemScope]("pg_catalog", "int8", nil))}})
			table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: schema + ".items", RelationScope: q.ByScopeColumn()})
			source := q.MapTable(table).As("i")
			id := q.Export("value", q.FieldOf(source.Rel(), "id", q.Int8()).Expr())
			d := q.Derived("d", q.Select(id.Project()).From(source))
			out := q.DefineView(reg, q.ViewSpec[composedValue]{Name: "entries"})
			plan := q.DefineRead(out, "visible", q.Select(q.Project(q.Output(out, "value", q.Int8()), id.From(d))).From(d))
			notes := q.DefinePlanSource(reg, q.TableSpec[note, int64]{Name: schema + ".notes", RelationScope: q.Global(), SoftDelete: &q.SoftDelete{DeletedValue: "closed"}})
			n := q.MapTable(notes).As("n")
			live := q.DefineRead(out, "live", q.Select(q.Project(q.Output(out, "value", q.Int8()), q.FieldOf(n.Rel(), "id", q.Int8()).Expr())).From(n).Take(10))
			shape := out.Definition().StatementShapes["visible.scoped.data"]
			for _, fn := range []string{`pg_catalog.int8eq("pg_catalog"."int8","pg_catalog"."int8")`, `pg_catalog.booleq("pg_catalog"."bool","pg_catalog"."bool")`} {
				require.Contains(t, shape.Privileges, q.DefinedPrivilege{Function: fn, Privilege: "EXECUTE"})
			}
			require.Contains(t, out.Definition().StatementShapes["live.global.data"].Privileges, q.DefinedPrivilege{Function: `pg_catalog.textne("pg_catalog"."text","pg_catalog"."text")`, Privilege: "EXECUTE"})
			body := func(ctx context.Context, s q.Session[itemScope]) error {
				rows, err := plan.Bind(s).Find(ctx)
				require.NoError(t, err)
				require.Equal(t, []composedValue{{new(int64(1))}}, rows)
				rows, err = live.Bind(s).Find(ctx)
				require.NoError(t, err)
				require.Equal(t, []composedValue{{new(int64(1))}}, rows)
				return nil
			}
			const shadowed = "SELECT 1::bigint = 2::bigint, true = false, 'open'::text <> 'open'::text"
			var integerEqual, boolEqual, textDifferent bool
			if driver == "stdsql" {
				require.NoError(t, readDB.QueryRowContext(t.Context(), shadowed).Scan(&integerEqual, &boolEqual, &textDifferent))
				r, err := stdsql.NewRunner(readDB, stdsql.ScopePolicy[itemScope]{}, stdsql.RunnerOptions{SQLAuthority: reg.SQLAuthority()})
				require.NoError(t, err)
				require.NoError(t, r.Within(t.Context(), 1, func(ctx context.Context, s *stdsql.Session[itemScope]) error { return body(ctx, s) }))
			} else {
				require.NoError(t, pool.QueryRow(t.Context(), shadowed).Scan(&integerEqual, &boolEqual, &textDifferent))
				r, err := pgxdriver.NewRunner(pool, pgxdriver.ScopePolicy[itemScope]{}, pgxdriver.RunnerOptions{SQLAuthority: reg.SQLAuthority()})
				require.NoError(t, err)
				require.NoError(t, r.Within(t.Context(), 1, func(ctx context.Context, s *pgxdriver.Session[itemScope]) error { return body(ctx, s) }))
			}
			require.True(t, integerEqual)
			require.True(t, boolEqual)
			require.True(t, textDifferent)
		})
	}
}
