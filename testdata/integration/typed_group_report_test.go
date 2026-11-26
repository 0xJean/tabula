//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

func TestGroupedReadUsesScalarValuesAndIndependentCount(t *testing.T) {
	type item struct {
		ID    int64  `db:"id,key"`
		Label string `db:"label"`
	}
	type result struct {
		Label  string `db:"label"`
		Amount int64  `db:"amount"`
	}

	db, pool := databases(t)
	tableName := typesSchema(t, db) + ".items"
	_, err := db.ExecContext(t.Context(), "CREATE TABLE "+tableName+` (id bigint PRIMARY KEY, label text COLLATE "C" NOT NULL)`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "INSERT INTO "+tableName+" VALUES (1,'A'),(2,'a'),(3,'B'),(4,'C')")
	require.NoError(t, err)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			cfg := q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL}
			if driver == "pq" {
				cfg.ArrayArg = pqdriver.Array
			}
			reg := q.NewRegistry(cfg)
			table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: tableName, RelationScope: q.Global()})
			layout := q.MapTable(table)
			src := layout.As("i")
			id := q.MappedColumn[int64](layout, "id").On(src.Rel())
			key := q.Lower(q.MappedColumn[string](layout, "label").On(src.Rel()).Expr())
			count := q.CountAll()
			one, zero := q.Input("one", q.Int8()), q.Input("zero", q.Int8())
			ids := q.Input("ids", q.ArrayOf(q.Int8()))
			value := q.Case(q.When(q.Gt(count, one.Expr()), q.Coalesce(q.NullIf(count, one.Expr()), zero.Expr()))).Else(zero.Expr())
			out := q.DefineView(reg, q.ViewSpec[result]{Name: "entries"})
			plan := q.DefineRead(out, "summary", q.Select(
				q.Project(q.Output(out, "label", q.Text()), key),
				q.Project(q.Output(out, "amount", q.Int8()), value),
			).From(src).GroupBy(key).WithList(q.ReadListSpec{
				Optional: []q.RowPredicate{{Name: "selected", When: q.Any(id.Expr(), ids.Expr())}},
				Orders:   []q.RowOrder{{Name: "by_label", Terms: []q.RowTerm{q.AscExpr(key)}}},
				Pages:    q.RowPages{Keyset: true, MaxLimit: 2}, ExactCount: true,
			}))
			for name, shape := range out.Definition().StatementShapes {
				require.Contains(t, shape.Privileges, q.DefinedPrivilege{Function: "pg_catalog.count()", Privilege: "EXECUTE"}, name)
			}
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				list := plan.Bind(s)
				choice := q.RowChoice{Filters: []string{"selected"}, Order: "by_label", Page: q.FirstPage, Limit: 2}
				values := []int64{1, 2, 3, 4}
				selected := q.BindValue(ids, values)
				values[0] = 9
				bindings := []q.Binding{selected, q.BindValue(one, int64(1)), q.BindValue(zero, int64(0))}
				first, total, err := list.List(ctx, choice, bindings...)
				require.NoError(t, err)
				require.Equal(t, []result{{"a", 2}, {"b", 0}}, first)
				require.EqualValues(t, 3, total)
				choice.Page = q.AfterPage
				choice.After, err = plan.CursorFrom(choice.Order, first[len(first)-1])
				require.NoError(t, err)
				last, total, err := list.List(ctx, choice, bindings...)
				require.NoError(t, err)
				require.Equal(t, []result{{"c", 0}}, last)
				require.EqualValues(t, 3, total)
				total, err = list.Count(ctx, choice.Filters, selected)
				require.NoError(t, err)
				require.EqualValues(t, 3, total)
				return nil
			}))
		})
	}
}

func TestMappedRowAndReadKeysetShareCatalogProof(t *testing.T) {
	type item struct {
		ID    int64  `db:"id,key"`
		Label string `db:"label"`
	}
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ns := typesSchema(t, db)
			physical := ns + ".items"
			_, err := db.ExecContext(t.Context(), "CREATE TABLE "+physical+` (id bigint PRIMARY KEY, label text COLLATE "C" NOT NULL)`)
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), "INSERT INTO "+physical+" VALUES (1,'first'),(2,'second')")
			require.NoError(t, err)
			var role string
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT CURRENT_USER").Scan(&role))
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: physical, RelationScope: q.Global()})
			layout := q.MapTable(table)
			src := layout.As("i")
			id := q.MappedColumn[int64](layout, "id").On(src.Rel())
			label := q.MappedColumn[string](layout, "label").On(src.Rel())
			orders := []q.RowOrder{{Name: "key", Terms: []q.RowTerm{q.AscExpr(label.Expr()).NullsLast(), q.Asc(id)}}}
			pages := q.RowPages{Keyset: true, MaxLimit: 2}
			rows := q.DefineTableRows(src, "rows", q.RowsSpec{Orders: orders, Pages: pages}, q.SelectAll())
			read := q.DefineTableRead(src, "selection", q.SelectAll(), q.Select().WithList(q.ReadListSpec{Orders: orders, Pages: pages}))
			def := table.Definition()
			a := def.StatementShapes["rows.global.data.sort.key.after"]
			b := def.StatementShapes["selection.global.list.data.sort.key.after"]
			require.NotEmpty(t, a.Keys)
			require.Equal(t, a.Keys, b.Keys)
			require.NotEmpty(t, a.Comparisons)
			require.Equal(t, a.Comparisons, b.Comparisons)
			work := sqlcheck.RoleStatements{}
			work.AddDefinitions(role, def)
			needs, err := work.Readiness(role)
			require.NoError(t, err)
			missing, err := sqlcheck.CheckReadiness(t.Context(), db, role, needs)
			require.NoError(t, err)
			require.Empty(t, missing)
			run := authorizedEffectRunner(t, driver, db, pool, role, reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				choice := q.RowChoice{Order: "key", Limit: 1, Page: q.FirstPage}
				first, err := rows.Bind(s).Find(ctx, choice)
				require.NoError(t, err)
				require.Equal(t, []item{{1, "first"}}, first)
				choice.Page = q.AfterPage
				choice.After, err = rows.CursorFrom(choice.Order, first[0])
				require.NoError(t, err)
				last, err := rows.Bind(s).Find(ctx, choice)
				require.NoError(t, err)
				choice.After, err = read.CursorFrom(choice.Order, first[0])
				require.NoError(t, err)
				got, err := read.Bind(s).Page(ctx, choice)
				require.NoError(t, err)
				require.Equal(t, []item{{2, "second"}}, last)
				require.Equal(t, last, got)
				return nil
			}))
			_, err = db.ExecContext(t.Context(), "ALTER TABLE "+physical+" DROP CONSTRAINT items_pkey")
			require.NoError(t, err)
			missing, err = sqlcheck.CheckReadiness(t.Context(), db, role, needs)
			require.NoError(t, err)
			require.Len(t, missing, 1)
			require.Equal(t, "KEY", missing[0].Kind)
			failures, err := sqlcheck.VerifyRoleStatements(t.Context(), db, work)
			require.NoError(t, err)
			require.Len(t, failures, 4) // first and after pages from each plan
		})
	}
}

func TestPhysicalNameColumnRetainsFieldOrder(t *testing.T) {
	type item struct {
		ID    int64  `db:"id,key"`
		Label string `db:"label"`
	}
	db, pool := databases(t)
	name := typesSchema(t, db) + ".items"
	_, err := db.ExecContext(t.Context(), "CREATE TABLE "+name+" (id bigint PRIMARY KEY, label pg_catalog.name NOT NULL)")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "INSERT INTO "+name+" VALUES (1,'b'),(2,'a'),(3,'a')")
	require.NoError(t, err)
	reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
	table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: name, RelationScope: q.Global()})
	layout := q.MapTable(table, q.ColumnType("label", q.CatalogType[string]("pg_catalog", "name", nil)))
	src := layout.As("i")
	label := q.MappedColumn[string](layout, "label").On(src.Rel())
	plan := q.DefineTableRows(src, "labels", q.RowsSpec{GroupBy: []q.RowColumn{label.Column()}, Orders: []q.RowOrder{{Name: "labels", Terms: []q.RowTerm{q.Asc(label)}}}}, q.SelectColumns(label.Column()))
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				items, total, err := plan.Bind(s).List(ctx, q.RowChoice{Order: "labels"})
				require.NoError(t, err)
				require.Equal(t, []item{{Label: "a"}, {Label: "b"}}, items)
				require.EqualValues(t, 2, total)
				return nil
			}))
		})
	}
}

func TestGroupedNumericCursorUsesTheDeclaredEquality(t *testing.T) {
	type item struct {
		ID     int64  `db:"id,key"`
		Amount string `db:"amount"`
	}
	type result struct {
		Amount   string `db:"amount"`
		Spelling string `db:"spelling"`
	}
	db, pool := databases(t)
	name := typesSchema(t, db) + ".items"
	_, err := db.ExecContext(t.Context(), "CREATE TABLE "+name+" (id bigint PRIMARY KEY, amount numeric NOT NULL)")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "INSERT INTO "+name+" VALUES (1,1.0),(2,1.00),(3,2.0)")
	require.NoError(t, err)
	reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
	number := q.CatalogType[string]("pg_catalog", "numeric", nil)
	table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: name, RelationScope: q.Global()})
	layout := q.MapTable(table, q.ColumnType("amount", number))
	src := layout.As("i")
	amount := q.MappedColumn[string](layout, "amount").On(src.Rel())
	spelling := q.Cast(q.Text(), amount.Expr())
	out := q.DefineView(reg, q.ViewSpec[result]{Name: "entries"})
	project := []q.Projection{
		q.Project(q.Output(out, "amount", number), amount.Expr()),
		q.Project(q.Output(out, "spelling", q.Text()), spelling),
	}
	orders := []q.RowOrder{{Name: "amount", Terms: []q.RowTerm{q.AscExpr(spelling), q.Asc(amount)}}}
	pages := q.RowPages{Keyset: true, MaxLimit: 1}
	list := q.ReadListSpec{Orders: orders, Pages: pages}
	base := q.Select(project...).From(src)
	_, err = q.TryDefineRead(out, "ambiguous", base.GroupBy(amount.Expr()).WithList(list))
	require.ErrorContains(t, err, "representation of equal GROUP BY values")
	_, err = q.TryDefineRows(out, "ambiguous_rows", src, q.RowsSpec{
		GroupBy: []q.RowColumn{amount.Column()}, Orders: orders, Pages: pages,
	}, project...)
	require.ErrorContains(t, err, "representation of equal GROUP BY values")
	// The spelling has its own fixed equality class when it is also grouped.
	_, err = q.TryDefineRead(out, "by_spelling", base.GroupBy(amount.Expr(), spelling).WithList(list))
	require.NoError(t, err)
	list.Orders = []q.RowOrder{{Name: "amount", Terms: []q.RowTerm{q.Asc(amount)}}}
	plan := q.DefineRead(out, "native", base.GroupBy(amount.Expr()).WithList(list))
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				choice := q.RowChoice{Page: q.FirstPage, Order: "amount", Limit: 1}
				first, err := plan.Bind(s).Page(ctx, choice)
				require.NoError(t, err)
				require.Len(t, first, 1)
				require.Contains(t, []string{"1.0", "1.00"}, first[0].Spelling)
				choice.After, err = plan.CursorFrom("amount", first[0])
				require.NoError(t, err)
				choice.Page = q.AfterPage
				last, err := plan.Bind(s).Page(ctx, choice)
				require.NoError(t, err)
				require.Len(t, last, 1)
				require.Equal(t, "2.0", last[0].Spelling)
				return nil
			}))
		})
	}
}
