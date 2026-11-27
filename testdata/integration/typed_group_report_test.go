//go:build integration

package integration_test

import (
	"context"
	"testing"

	q "github.com/0xJean/tabula"
	tplan "github.com/0xJean/tabula/plan"
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
			layout := tplan.MapTable(table)
			src := layout.As("i")
			id := tplan.MappedColumn[int64](layout, "id").On(src.Rel())
			key := tplan.Lower(tplan.MappedColumn[string](layout, "label").On(src.Rel()).Expr())
			count := tplan.CountAll()
			one, zero := tplan.Input("one", tplan.Int8()), tplan.Input("zero", tplan.Int8())
			ids := tplan.Input("ids", q.ArrayOf(tplan.Int8()))
			value := tplan.Case(tplan.When(tplan.Gt(count, one.Expr()), tplan.Coalesce(tplan.NullIf(count, one.Expr()), zero.Expr()))).Else(zero.Expr())
			out := q.DefineView(reg, q.ViewSpec[result]{Name: "entries"})
			plan := tplan.DefineRead(out, "summary", tplan.Select(
				tplan.Project(tplan.Output(out, "label", tplan.Text()), key),
				tplan.Project(tplan.Output(out, "amount", tplan.Int8()), value),
			).From(src).GroupBy(key).WithList(tplan.ReadListSpec{
				Optional: []tplan.RowPredicate{{Name: "selected", When: tplan.Any(id.Expr(), ids.Expr())}},
				Orders:   []tplan.RowOrder{{Name: "by_label", Terms: []tplan.RowTerm{tplan.AscExpr(key)}}},
				Pages:    tplan.RowPages{Keyset: true, MaxLimit: 2}, ExactCount: true,
			}))
			for name, shape := range out.Definition().StatementShapes {
				require.Contains(t, shape.Privileges, q.DefinedPrivilege{Function: "pg_catalog.count()", Privilege: "EXECUTE"}, name)
			}
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				list := plan.Bind(s)
				choice := tplan.RowChoice{Filters: []string{"selected"}, Order: "by_label", Page: tplan.FirstPage, Limit: 2}
				values := []int64{1, 2, 3, 4}
				selected := tplan.BindValue(ids, values)
				values[0] = 9
				bindings := []tplan.Binding{selected, tplan.BindValue(one, int64(1)), tplan.BindValue(zero, int64(0))}
				first, total, err := list.List(ctx, choice, bindings...)
				require.NoError(t, err)
				require.Equal(t, []result{{"a", 2}, {"b", 0}}, first)
				require.EqualValues(t, 3, total)
				choice.Page = tplan.AfterPage
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
			layout := tplan.MapTable(table)
			src := layout.As("i")
			id := tplan.MappedColumn[int64](layout, "id").On(src.Rel())
			label := tplan.MappedColumn[string](layout, "label").On(src.Rel())
			orders := []tplan.RowOrder{{Name: "key", Terms: []tplan.RowTerm{tplan.AscExpr(label.Expr()).NullsLast(), tplan.Asc(id)}}}
			pages := tplan.RowPages{Keyset: true, MaxLimit: 2}
			rows := tplan.DefineTableRows(src, "rows", tplan.RowsSpec{Orders: orders, Pages: pages}, tplan.SelectAll())
			read := tplan.DefineTableRead(src, "selection", tplan.SelectAll(), tplan.Select().WithList(tplan.ReadListSpec{Orders: orders, Pages: pages}))
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
				choice := tplan.RowChoice{Order: "key", Limit: 1, Page: tplan.FirstPage}
				first, err := rows.Bind(s).Find(ctx, choice)
				require.NoError(t, err)
				require.Equal(t, []item{{1, "first"}}, first)
				choice.Page = tplan.AfterPage
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
	layout := tplan.MapTable(table, tplan.ColumnType("label", q.CatalogType[string]("pg_catalog", "name", nil)))
	src := layout.As("i")
	label := tplan.MappedColumn[string](layout, "label").On(src.Rel())
	plan := tplan.DefineTableRows(src, "labels", tplan.RowsSpec{GroupBy: []tplan.RowColumn{label.Column()}, Orders: []tplan.RowOrder{{Name: "labels", Terms: []tplan.RowTerm{tplan.Asc(label)}}}}, tplan.SelectColumns(label.Column()))
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				items, total, err := plan.Bind(s).List(ctx, tplan.RowChoice{Order: "labels"})
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
	layout := tplan.MapTable(table, tplan.ColumnType("amount", number))
	src := layout.As("i")
	amount := tplan.MappedColumn[string](layout, "amount").On(src.Rel())
	spelling := tplan.Cast(tplan.Text(), amount.Expr())
	out := q.DefineView(reg, q.ViewSpec[result]{Name: "entries"})
	project := []tplan.Projection{
		tplan.Project(tplan.Output(out, "amount", number), amount.Expr()),
		tplan.Project(tplan.Output(out, "spelling", tplan.Text()), spelling),
	}
	orders := []tplan.RowOrder{{Name: "amount", Terms: []tplan.RowTerm{tplan.AscExpr(spelling), tplan.Asc(amount)}}}
	pages := tplan.RowPages{Keyset: true, MaxLimit: 1}
	list := tplan.ReadListSpec{Orders: orders, Pages: pages}
	base := tplan.Select(project...).From(src)
	_, err = tplan.TryDefineRead(out, "ambiguous", base.GroupBy(amount.Expr()).WithList(list))
	require.ErrorContains(t, err, "representation of equal GROUP BY values")
	_, err = tplan.TryDefineRows(out, "ambiguous_rows", src, tplan.RowsSpec{
		GroupBy: []tplan.RowColumn{amount.Column()}, Orders: orders, Pages: pages,
	}, project...)
	require.ErrorContains(t, err, "representation of equal GROUP BY values")
	// The spelling has its own fixed equality class when it is also grouped.
	_, err = tplan.TryDefineRead(out, "by_spelling", base.GroupBy(amount.Expr(), spelling).WithList(list))
	require.NoError(t, err)
	list.Orders = []tplan.RowOrder{{Name: "amount", Terms: []tplan.RowTerm{tplan.Asc(amount)}}}
	plan := tplan.DefineRead(out, "native", base.GroupBy(amount.Expr()).WithList(list))
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			require.NoError(t, run(t.Context(), func(ctx context.Context, s session) error {
				choice := tplan.RowChoice{Page: tplan.FirstPage, Order: "amount", Limit: 1}
				first, err := plan.Bind(s).Page(ctx, choice)
				require.NoError(t, err)
				require.Len(t, first, 1)
				require.Contains(t, []string{"1.0", "1.00"}, first[0].Spelling)
				choice.After, err = plan.CursorFrom("amount", first[0])
				require.NoError(t, err)
				choice.Page = tplan.AfterPage
				last, err := plan.Bind(s).Page(ctx, choice)
				require.NoError(t, err)
				require.Len(t, last, 1)
				require.Equal(t, "2.0", last[0].Spelling)
				return nil
			}))
		})
	}
}
