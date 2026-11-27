package plan

import (
	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/internal/planrt"

	"testing"

	"github.com/stretchr/testify/require"
)

type typedItem struct {
	ID    int64   `db:"id,key"`
	Label *string `db:"label,secret"`
}

type typedResult struct {
	ID int64 `db:"id"`
}

func TestTypedFieldsAndBindings(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[customTenant]{})
	tab := tabula.Define(reg, tabula.TableSpec[typedItem, int64]{Name: `"public"."items"`, RelationScope: tabula.ByRLS()})
	r := FromTable(tab, "i", tabula.ByRLS())
	id := FieldOf(r, "id", Int8())
	in := Input("id", Int8())
	label := Input("label", Text())
	asUUID := tabula.CatalogType[string]("pg_catalog", "uuid", nil)
	other := FromTable(tab, "i", tabula.ByRLS())
	view := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "notes"})
	out := Output(view, "id", Int8())

	require.Panics(t, func() { FromTable(tab, "i", tabula.Global()) })
	require.Panics(t, func() { FieldOf(r, "absent", Text()) })
	require.Panics(t, func() { FieldOf(r, "id", Text()) })
	comp, err := newTypedCompiler(true, r.source)
	require.NoError(t, err)
	scopePredicate, err := comp.scopePredicate(r.source, catalogType{})
	require.NoError(t, err)
	require.Empty(t, scopePredicate)
	selects, fields, err := comp.project(ownerOutputLayout(planView(view)), []Projection{Project(out, id.Expr())})
	require.NoError(t, err)
	require.Equal(t, []string{`"i"."id" AS "id"`}, selects)
	require.Equal(t, []tabula.ShapeField{{Name: "id", Type: `"pg_catalog"."int8"`}}, fields)

	filter := And(Eq(id.Expr(), in.Expr()), IsNotDistinct(FieldOf(r, "label", Text()).Expr(), label.Expr()), Eq(id.Expr(), in.Expr()))
	sql, err := filter.part.render(comp)
	require.NoError(t, err)
	require.Equal(t, `(("i"."id" = CAST($1 AS "pg_catalog"."int8")) AND ("i"."label" IS NOT DISTINCT FROM CAST($2 AS "pg_catalog"."text")) AND ("i"."id" = CAST($1 AS "pg_catalog"."int8")))`, sql)
	args, err := comp.params.bind(nil, []Binding{BindValue(label, "value"), BindValue(in, 9)})
	require.NoError(t, err)
	require.Equal(t, []any{int64(9), "value"}, args)
	meta, err := comp.readRequirements()
	require.NoError(t, err)
	shape, err := comp.finishShape("filtered", "SELECT "+selects[0]+" FROM "+r.source.table.SQL()+` AS "i" WHERE `+sql, catalogType{}, meta)
	require.NoError(t, err)
	require.Len(t, shape.shape.Inputs, 2)
	require.Equal(t, []tabula.ShapeColumn{
		{Relation: r.source.table.SQL(), Name: "id", Type: typeID(Int8()).Qualified()}, {Relation: r.source.table.SQL(), Name: "label", Type: typeID(Text()).Qualified()},
	}, shape.shape.PhysicalColumns)
	for _, param := range shape.shape.Inputs {
		require.True(t, param.Secret)
	}
	otherInput := Input("id", Int8())
	for _, bad := range [][]Binding{
		{BindValue(in, 9)},
		{BindValue(in, 9), BindValue(label, "one"), BindValue(label, "two")},
		{BindValue(otherInput, 9), BindValue(label, "one")},
		{{}},
	} {
		args, err = comp.params.bind(nil, bad)
		require.Error(t, err)
		require.Nil(t, args)
	}
	_, err = otherInput.Expr().part.render(comp)
	require.ErrorContains(t, err, "distinct inputs")
	_, err = Eq(FieldOf(other, "id", Int8()).Expr(), in.Expr()).part.render(comp)
	require.ErrorContains(t, err, "outside")
	_, err = Eq(FieldOf(r, "label", asUUID).Expr(), label.Expr()).part.render(comp)
	require.ErrorContains(t, err, "same declared PostgreSQL type")

	type scopedItem struct {
		ID    int64        `db:"id,key"`
		Scope customTenant `db:"scope_id,scope"`
	}
	st := tabula.Define(reg, tabula.TableSpec[scopedItem, int64]{Name: "public.jobs"})
	sr := FromTable(st, "j", tabula.ByScopeColumn())
	scoped, err := newTypedCompiler(true, sr.source)
	require.NoError(t, err)
	scopePredicate, err = scoped.scopePredicate(sr.source, typeID(tabula.CatalogType[customTenant]("pg_catalog", "text", nil)))
	require.NoError(t, err)
	require.Equal(t, `("j"."scope_id" = CAST($1 AS "pg_catalog"."text"))`, scopePredicate)
	slot, err := in.Expr().part.render(scoped)
	require.NoError(t, err)
	require.Equal(t, `CAST($2 AS "pg_catalog"."int8")`, slot)
	args, err = scoped.params.bind("scope", []Binding{BindValue(in, 7)})
	require.NoError(t, err)
	require.Equal(t, []any{"scope", int64(7)}, args)
}

func TestTypedShapeRegistrationAndAuthority(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[customTenant]{})
	tab := tabula.Define(reg, tabula.TableSpec[typedItem, int64]{Name: "public.items", RelationScope: tabula.ByRLS()})
	src := FromTable(tab, "i", tabula.ByRLS())
	id := FieldOf(src, "id", Int8())
	view := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "entries"})
	c, err := newTypedCompiler(true, src.source)
	require.NoError(t, err)
	columns, fields, err := c.project(ownerOutputLayout(planView(view)), []Projection{Project(Output(view, "id", Int8()), id.Expr())})
	require.NoError(t, err)
	slot := Input("id", Int8())
	where, err := Eq(id.Expr(), slot.Expr()).part.render(c)
	require.NoError(t, err)
	physical := src.source.table.SQL()
	key := "by_id"
	meta, err := c.readRequirements()
	require.NoError(t, err)
	meta.Result = fields
	require.Equal(t, []tabula.DefinedPrivilege{{Table: physical, Privilege: "SELECT", Columns: []string{"id"}}}, meta.Privileges)
	query := "SELECT " + columns[0] + " FROM " + physical + ` AS "i" WHERE ` + where
	shape, err := c.finishShape(key, query, catalogType{}, meta)
	require.NoError(t, err)
	require.Equal(t, []string{physical}, shape.shape.ForceRLS)
	require.Equal(t, []string{"pg_catalog", "public"}, shape.shape.SchemaUsage)
	require.Equal(t, []tabula.ShapeInput{{Name: "id", Type: typeID(Int8()).Qualified()}}, shape.shape.Inputs)
	extra := Input("later", Int8())
	_, err = extra.Expr().part.render(c)
	require.NoError(t, err)
	require.Len(t, shape.layout.order, 1)
	s := new(fakeSession)
	_, err = bindShapeCall(planView(view), shape, s, tabula.CallEach, nil, false, false)
	require.ErrorIs(t, err, tabula.ErrStatementAuthority)
	require.NoError(t, registerShapes(planView(view), key, []*compiledShape{shape}))
	snap := reg.DefinedViews()[0]
	require.Equal(t, key, snap.StatementQueries[key])
	require.Equal(t, `SELECT "i"."id" AS "id" FROM "public"."items" AS "i" WHERE ("i"."id" = CAST($1 AS "pg_catalog"."int8"))`, snap.Statements[key])
	snap.StatementShapes[key].Privileges[0].Columns[0] = "changed"
	snap.StatementShapes[key].ForceRLS[0] = "changed"
	snap.StatementShapes[key].PhysicalColumns[0].Name = "changed"
	require.Equal(t, shape.shape, reg.DefinedViews()[0].StatementShapes[key])

	call, err := bindShapeCall(planView(view), shape, s, tabula.CallEach, []Binding{BindValue(slot, 5)}, false, false)
	require.NoError(t, err)
	auth := reg.SQLAuthority()
	sql, err := auth.Open(s, tabula.CallEach, call)
	require.NoError(t, err)
	require.Equal(t, []any{int64(5)}, sql.Args)
	require.Equal(t, []bool{false}, sql.SecretArgs)
	require.Equal(t, "entries.by_id", sql.Key)
	sql.Args[0] = "changed"
	sql.SecretArgs[0] = true
	sql, err = auth.Open(s, tabula.CallEach, call)
	require.NoError(t, err)
	require.Equal(t, int64(5), sql.Args[0])
	require.Equal(t, []bool{false}, sql.SecretArgs)
	_, err = auth.Open(new(fakeSession), tabula.CallEach, call)
	require.ErrorIs(t, err, tabula.ErrStatementAuthority)
	_, err = auth.Open(s, tabula.CallExec, call)
	require.ErrorIs(t, err, tabula.ErrStatementAuthority)
	_, err = bindShapeCall(planView(view), shape, s, tabula.CallExec, []Binding{BindValue(slot, 5)}, false, false)
	require.ErrorIs(t, err, tabula.ErrStatementAuthority)
	_, err = auth.Open(s, tabula.CallEach, tabula.AuthorizedStatement{})
	require.ErrorIs(t, err, tabula.ErrStatementAuthority)
	_, err = tabula.NewRegistry(tabula.RegistryConfig[customTenant]{}).SQLAuthority().Open(s, tabula.CallEach, call)
	require.ErrorIs(t, err, tabula.ErrStatementAuthority)

	foreign := tabula.NewRegistry(tabula.RegistryConfig[customTenant]{})
	foreignView := tabula.DefineView(foreign, tabula.ViewSpec[typedResult]{Name: "entries"})
	unregistered := *shape
	unregistered.compiled = &planrt.CompiledShape{Key: shape.compiled.Key, SQL: shape.compiled.SQL, Effects: shape.compiled.Effects, HasResult: shape.compiled.HasResult}
	require.ErrorContains(t, registerShapes(planView(foreignView), key, []*compiledShape{&unregistered}), "foreign physical relation")

	conflict := *shape
	conflict.compiled = &planrt.CompiledShape{Key: "other", SQL: shape.compiled.SQL, Effects: shape.compiled.Effects, HasResult: shape.compiled.HasResult}
	conflict.columns = map[physicalColumn]catalogType{{Table: src.source.table, Name: "id"}: typeID(Text())}
	pending := *shape
	pending.compiled = &planrt.CompiledShape{Key: "pending", SQL: shape.compiled.SQL, Effects: shape.compiled.Effects, HasResult: shape.compiled.HasResult}
	existing := view.Definition()
	require.ErrorContains(t, registerShapes(planView(view), "later", []*compiledShape{&pending, &conflict}), "conflicting PostgreSQL types")
	require.Equal(t, existing, view.Definition())
	require.Nil(t, pending.compiled.Authority)
}

func TestTypedVariantBudget(t *testing.T) {
	choices := []shapeChoice{{"filter", []string{"all", "equal"}}, {"order", []string{"id", "label", "rank"}}}
	over, err := enumerateShapes("items", 4, choices, nil)
	require.Nil(t, over)
	require.ErrorContains(t, err, "filter=equal, order=label")
	require.ErrorContains(t, err, "filter=2, order=3")
	finite, err := enumerateShapes("items", 4, choices, func(prefix []shapeAlternative) bool {
		return len(prefix) < 2 || prefix[0].value != "all" || prefix[1].value == "id"
	})
	require.NoError(t, err)
	require.Len(t, finite, 4)
	require.Equal(t, []shapeAlternative{{"filter", "equal"}, {"order", "rank"}}, finite[3])
	finite, err = enumerateShapes("base", 0, nil, nil)
	require.NoError(t, err)
	require.Len(t, finite, 1)
	require.Empty(t, finite[0])
}

func TestTypedCompilerRelationIdentity(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[customTenant]{})
	spec := tabula.TableSpec[typedItem, int64]{Name: "public.items", RelationScope: tabula.Global()}
	table := tabula.Define(reg, spec)
	a := FromTable(table, "i", tabula.Global())
	b := FromTable(table, "j", tabula.Global())
	sameAlias := FromTable(table, "i", tabula.Global())
	other := FromTable(tabula.Define(tabula.NewRegistry(tabula.RegistryConfig[customTenant]{}), spec), "k", tabula.Global())
	for _, test := range []struct {
		sources []*occurrence
		message string
	}{
		{[]*occurrence{nil}, "undeclared relation"},
		{[]*occurrence{a.source, a.source}, "duplicate relation alias"},
		{[]*occurrence{a.source, sameAlias.source}, "duplicate relation alias"},
		{[]*occurrence{a.source, other.source}, "different registries"},
	} {
		c, err := newTypedCompiler(true, test.sources...)
		require.ErrorContains(t, err, test.message)
		require.Nil(t, c)
	}
	c, err := newTypedCompiler(true, a.source, b.source)
	require.NoError(t, err)
	meta, err := c.readRequirements()
	require.NoError(t, err)
	require.Equal(t, []tabula.ShapeAccess{{Relation: a.source.table.SQL(), Command: "SELECT"}}, meta.Access)
	require.Equal(t, []string{a.source.table.SQL()}, meta.AnyColumnSelect)
}

func TestTypedColumnScopeBinding(t *testing.T) {
	type scopedItem struct {
		ID    int64        `db:"id,key"`
		Scope customTenant `db:"scope_id,scope"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[customTenant]{})
	tab := tabula.Define(reg, tabula.TableSpec[scopedItem, int64]{Name: "public.items"})
	src := FromTable(tab, "i", tabula.ByScopeColumn()).source
	result := []tabula.ShapeField{{Name: "count", Type: typeID(Int8()).Qualified()}}
	for _, scoped := range []bool{true, false} {
		c, err := newTypedCompiler(scoped, src)
		require.NoError(t, err)
		sql := "SELECT count(*) FROM " + src.table.SQL() + ` AS "i"`
		scopeType := tabula.CatalogType[customTenant]("pg_catalog", "text", nil)
		if scoped {
			_, err = c.finishShape("missing", sql, typeID(scopeType), tabula.StatementShape{Effects: tabula.EffectRead})
			require.ErrorContains(t, err, "missing scope predicate")
		}
		filter, err := c.scopePredicate(src, typeID(scopeType))
		require.NoError(t, err)
		key := "count_all_items"
		if scoped {
			key = "count_scoped_items"
			sql += " WHERE " + filter
		}
		meta, err := c.readRequirements()
		require.NoError(t, err)
		meta.Result = result
		shape, err := c.finishShape(key, sql, typeID(scopeType), meta)
		require.NoError(t, err)
		require.Equal(t, !scoped, shape.shape.UnscopedOnly)
		require.NoError(t, registerShapes(planTable(tab).Owner, key, []*compiledShape{shape}))
		mismatch := &fakeSession{unscoped: scoped}
		_, err = bindShapeCall(planTable(tab).Owner, shape, mismatch, tabula.CallScan, nil, false, false)
		require.ErrorIs(t, err, tabula.ErrStatementAuthority)
		match := &fakeSession{tenant: "group", unscoped: !scoped}
		call, err := bindShapeCall(planTable(tab).Owner, shape, match, tabula.CallScan, nil, false, false)
		require.NoError(t, err)
		query, err := reg.SQLAuthority().Open(match, tabula.CallScan, call)
		require.NoError(t, err)
		if scoped {
			require.Equal(t, []any{customTenant("group")}, query.Args)
			require.Equal(t, []tabula.ShapeColumn{{Relation: src.table.SQL(), Name: "scope_id", Type: typeID(scopeType).Qualified()}}, shape.shape.PhysicalColumns)
		} else {
			require.Empty(t, query.Args)
			require.Empty(t, shape.shape.PhysicalColumns)
		}
	}
}

func TestTypedShapeTableWriteModes(t *testing.T) {
	type item struct {
		ID    int64  `db:"id,key"`
		Label string `db:"label"`
		Rank  int64  `db:"rank"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[customTenant]{})
	insert := tabula.Define(reg, tabula.TableSpec[item, int64]{Name: "public.entries", RelationScope: tabula.Global(), WriteMode: tabula.WriteInsertOnly})
	update := tabula.Define(reg, tabula.TableSpec[item, int64]{Name: "public.items", RelationScope: tabula.Global(), WriteMode: tabula.WriteUpdateOnly, UpdateColumns: []string{"label"}})
	owner := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "notes"})
	for _, table := range []*tabula.Table[item, int64, customTenant]{insert, update} {
		src := FromTable(table, "i", tabula.Global()).source
		c, err := newTypedCompiler(true, src)
		require.NoError(t, err)
		meta, err := c.readRequirements()
		physical := src.table.SQL()
		if table == insert {
			require.ErrorIs(t, err, tabula.ErrOperationDenied)
			meta = tabula.StatementShape{Effects: tabula.EffectRead, Access: []tabula.ShapeAccess{{Relation: physical, Command: "SELECT"}}}
		} else {
			require.NoError(t, err)
		}
		meta.Result = []tabula.ShapeField{{Name: "count", Type: typeID(Int8()).Qualified()}}
		read, err := c.finishShape("read_items", "SELECT count(*) FROM "+physical, catalogType{}, meta)
		require.NoError(t, err)
		err = registerShapes(planView(owner), "read_items", []*compiledShape{read})
		if table == insert {
			require.ErrorIs(t, err, tabula.ErrOperationDenied)
			require.Empty(t, owner.Definition().StatementShapes)
			require.Nil(t, read.compiled.Authority)
			continue
		}
		require.NoError(t, err)
		meta = tabula.StatementShape{Effects: tabula.EffectWrite, Access: []tabula.ShapeAccess{{Relation: physical, Command: "UPDATE"}}}
		for _, col := range []string{"rank", "label"} {
			meta.Privileges = []tabula.DefinedPrivilege{{Table: physical, Privilege: "UPDATE", Columns: []string{col}}}
			key := "update_" + col
			shape, err := c.finishShape(key, "UPDATE "+physical+" SET "+typedQuote(col)+" = DEFAULT", catalogType{}, meta)
			require.NoError(t, err)
			err = registerShapes(planView(owner), key, []*compiledShape{shape})
			if col == "label" {
				require.NoError(t, err)
				require.Contains(t, owner.Definition().Statements, key)
			} else {
				require.ErrorIs(t, err, tabula.ErrOperationDenied)
				require.NotContains(t, owner.Definition().Statements, key)
			}
		}
	}
}
