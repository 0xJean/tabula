package tabula

import (
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
	reg := NewRegistry(RegistryConfig[customTenant]{})
	tab := Define(reg, TableSpec[typedItem, int64]{Name: `"public"."items"`, RelationScope: ByRLS()})
	r := FromTable(tab, "i", ByRLS())
	id := FieldOf(r, "id", Int8())
	in := Input("id", Int8())
	label := Input("label", Text())
	asUUID := CatalogType[string]("pg_catalog", "uuid", nil)
	other := FromTable(tab, "i", ByRLS())
	view := DefineView(reg, ViewSpec[typedResult]{Name: "notes"})
	out := Output(view, "id", Int8())

	require.Panics(t, func() { FromTable(tab, "i", Global()) })
	require.Panics(t, func() { FieldOf(r, "absent", Text()) })
	require.Panics(t, func() { FieldOf(r, "id", Text()) })
	comp, err := newTypedCompiler(true, r.source)
	require.NoError(t, err)
	scopePredicate, err := comp.scopePredicate(r.source)
	require.NoError(t, err)
	require.Empty(t, scopePredicate)
	selects, fields, err := comp.project(view.entry, []Projection{Project(out, id.Expr())})
	require.NoError(t, err)
	require.Equal(t, []string{`"i"."id" AS "id"`}, selects)
	require.Equal(t, []ShapeField{{Name: "id", Type: `"pg_catalog"."int8"`}}, fields)

	filter := And(Eq(id.Expr(), in.Expr()), IsNotDistinct(FieldOf(r, "label", Text()).Expr(), label.Expr()), Eq(id.Expr(), in.Expr()))
	sql, err := filter.part.render(comp)
	require.NoError(t, err)
	require.Equal(t, `(("i"."id" = $1) AND ("i"."label" IS NOT DISTINCT FROM $2) AND ("i"."id" = $1))`, sql)
	args, err := comp.params.bind(nil, []Binding{BindValue(label, "value"), BindValue(in, 9)})
	require.NoError(t, err)
	require.Equal(t, []any{int64(9), "value"}, args)
	meta, err := comp.readRequirements()
	require.NoError(t, err)
	shape, err := comp.finishShape("filtered", "SELECT "+selects[0]+" FROM "+r.source.table.sql()+` AS "i" WHERE `+sql, catalogType{}, meta)
	require.NoError(t, err)
	require.Len(t, shape.shape.Inputs, 2)
	require.Equal(t, []ShapeColumn{
		{r.source.table.sql(), "id", Int8().id.qualified()}, {r.source.table.sql(), "label", Text().id.qualified()},
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
	st := Define(reg, TableSpec[scopedItem, int64]{Name: "public.jobs"})
	sr := FromTable(st, "j", ByScopeColumn())
	scoped, err := newTypedCompiler(true, sr.source)
	require.NoError(t, err)
	scopePredicate, err = scoped.scopePredicate(sr.source)
	require.NoError(t, err)
	require.Equal(t, `("j"."scope_id" = $1)`, scopePredicate)
	slot, err := in.Expr().part.render(scoped)
	require.NoError(t, err)
	require.Equal(t, "$2", slot)
	args, err = scoped.params.bind("scope", []Binding{BindValue(in, 7)})
	require.NoError(t, err)
	require.Equal(t, []any{"scope", int64(7)}, args)
}

func TestTypedShapeRegistrationAndAuthority(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	tab := Define(reg, TableSpec[typedItem, int64]{Name: "public.items", RelationScope: ByRLS()})
	src := FromTable(tab, "i", ByRLS())
	id := FieldOf(src, "id", Int8())
	view := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	c, err := newTypedCompiler(true, src.source)
	require.NoError(t, err)
	columns, fields, err := c.project(view.entry, []Projection{Project(Output(view, "id", Int8()), id.Expr())})
	require.NoError(t, err)
	slot := Input("id", Int8())
	where, err := Eq(id.Expr(), slot.Expr()).part.render(c)
	require.NoError(t, err)
	physical := src.source.table.sql()
	key := "by_id"
	meta, err := c.readRequirements()
	require.NoError(t, err)
	meta.Result = fields
	require.Equal(t, []DefinedPrivilege{{Table: physical, Privilege: "SELECT", Columns: []string{"id"}}}, meta.Privileges)
	query := "SELECT " + columns[0] + " FROM " + physical + ` AS "i" WHERE ` + where
	shape, err := c.finishShape(key, query, catalogType{}, meta)
	require.NoError(t, err)
	require.Equal(t, []string{physical}, shape.shape.ForceRLS)
	require.Equal(t, []string{"public"}, shape.shape.SchemaUsage)
	require.Equal(t, []ShapeInput{{Name: "id", Type: Int8().id.qualified()}}, shape.shape.Inputs)
	extra := Input("later", Int8())
	_, err = extra.Expr().part.render(c)
	require.NoError(t, err)
	require.Len(t, shape.layout.order, 1)
	s := new(fakeSession)
	_, err = reg.bindCall(shape, s, CallEach, nil)
	require.ErrorIs(t, err, ErrStatementAuthority)
	require.NoError(t, view.registerShapes(key, []*compiledShape{shape}))
	snap := reg.DefinedViews()[0]
	require.Equal(t, key, snap.StatementQueries[key])
	require.Equal(t, `SELECT "i"."id" AS "id" FROM "public"."items" AS "i" WHERE ("i"."id" = $1)`, snap.Statements[key])
	snap.StatementShapes[key].Privileges[0].Columns[0] = "changed"
	snap.StatementShapes[key].ForceRLS[0] = "changed"
	snap.StatementShapes[key].PhysicalColumns[0].Name = "changed"
	require.Equal(t, shape.shape, reg.DefinedViews()[0].StatementShapes[key])

	call, err := reg.bindCall(shape, s, CallEach, []Binding{BindValue(slot, 5)})
	require.NoError(t, err)
	auth := reg.SQLAuthority()
	sql, err := auth.Open(s, CallEach, call)
	require.NoError(t, err)
	require.Equal(t, []any{int64(5)}, sql.Args)
	require.Equal(t, []bool{false}, sql.SecretArgs)
	require.Equal(t, "entries.by_id", sql.Key)
	sql.Args[0] = "changed"
	sql.SecretArgs[0] = true
	sql, err = auth.Open(s, CallEach, call)
	require.NoError(t, err)
	require.Equal(t, int64(5), sql.Args[0])
	require.Equal(t, []bool{false}, sql.SecretArgs)
	_, err = auth.Open(new(fakeSession), CallEach, call)
	require.ErrorIs(t, err, ErrStatementAuthority)
	_, err = auth.Open(s, CallExec, call)
	require.ErrorIs(t, err, ErrStatementAuthority)
	_, err = reg.bindCall(shape, s, CallExec, []Binding{BindValue(slot, 5)})
	require.ErrorIs(t, err, ErrStatementAuthority)
	_, err = auth.Open(s, CallEach, AuthorizedStatement{})
	require.ErrorIs(t, err, ErrStatementAuthority)
	_, err = NewRegistry(RegistryConfig[customTenant]{}).SQLAuthority().Open(s, CallEach, call)
	require.ErrorIs(t, err, ErrStatementAuthority)

	foreign := NewRegistry(RegistryConfig[customTenant]{})
	foreignView := DefineView(foreign, ViewSpec[typedResult]{Name: "entries"})
	unregistered := *shape
	unregistered.authority = nil
	require.ErrorContains(t, foreignView.registerShapes(key, []*compiledShape{&unregistered}), "foreign physical relation")

	conflict := *shape
	conflict.key, conflict.authority = "other", nil
	conflict.columns = map[physicalColumn]catalogType{{src.source.table, "id"}: Text().id}
	pending := *shape
	pending.key, pending.authority = "pending", nil
	existing := view.Definition()
	require.ErrorContains(t, view.registerShapes("later", []*compiledShape{&pending, &conflict}), "conflicting PostgreSQL types")
	require.Equal(t, existing, view.Definition())
	require.Nil(t, pending.authority)
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
	reg := NewRegistry(RegistryConfig[customTenant]{})
	spec := TableSpec[typedItem, int64]{Name: "public.items", RelationScope: Global()}
	table := Define(reg, spec)
	a := FromTable(table, "i", Global())
	b := FromTable(table, "j", Global())
	sameAlias := FromTable(table, "i", Global())
	other := FromTable(Define(NewRegistry(RegistryConfig[customTenant]{}), spec), "k", Global())
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
	require.Equal(t, []ShapeAccess{{a.source.table.sql(), "SELECT"}}, meta.Access)
	require.Equal(t, []string{a.source.table.sql()}, meta.AnyColumnSelect)
}

func TestTypedColumnScopeBinding(t *testing.T) {
	type scopedItem struct {
		ID    int64        `db:"id,key"`
		Scope customTenant `db:"scope_id,scope"`
	}
	reg := NewRegistry(RegistryConfig[customTenant]{})
	tab := Define(reg, TableSpec[scopedItem, int64]{Name: "public.items"})
	src := FromTable(tab, "i", ByScopeColumn()).source
	result := []ShapeField{{Name: "count", Type: Int8().id.qualified()}}
	for _, scoped := range []bool{true, false} {
		c, err := newTypedCompiler(scoped, src)
		require.NoError(t, err)
		sql := "SELECT count(*) FROM " + src.table.sql() + ` AS "i"`
		scopeType := CatalogType[customTenant]("pg_catalog", "text", nil)
		if scoped {
			_, err = c.finishShape("missing", sql, scopeType.id, StatementShape{Effects: EffectRead})
			require.ErrorContains(t, err, "missing scope predicate")
		}
		filter, err := c.scopePredicate(src)
		require.NoError(t, err)
		key := "count_all_items"
		if scoped {
			key = "count_scoped_items"
			sql += " WHERE " + filter
		}
		meta, err := c.readRequirements()
		require.NoError(t, err)
		meta.Result = result
		shape, err := c.finishShape(key, sql, scopeType.id, meta)
		require.NoError(t, err)
		require.Equal(t, !scoped, shape.shape.UnscopedOnly)
		require.NoError(t, tab.registerShapes(key, []*compiledShape{shape}))
		mismatch := &fakeSession{unscoped: scoped}
		_, err = reg.bindCall(shape, mismatch, CallScan, nil)
		require.ErrorIs(t, err, ErrStatementAuthority)
		match := &fakeSession{tenant: "group", unscoped: !scoped}
		call, err := reg.bindCall(shape, match, CallScan, nil)
		require.NoError(t, err)
		query, err := reg.SQLAuthority().Open(match, CallScan, call)
		require.NoError(t, err)
		if scoped {
			require.Equal(t, []any{customTenant("group")}, query.Args)
			require.Equal(t, []ShapeColumn{{src.table.sql(), "scope_id", scopeType.id.qualified()}}, shape.shape.PhysicalColumns)
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
	reg := NewRegistry(RegistryConfig[customTenant]{})
	insert := Define(reg, TableSpec[item, int64]{Name: "public.entries", RelationScope: Global(), WriteMode: WriteInsertOnly})
	update := Define(reg, TableSpec[item, int64]{Name: "public.items", RelationScope: Global(), WriteMode: WriteUpdateOnly, UpdateColumns: []string{"label"}})
	owner := DefineView(reg, ViewSpec[typedResult]{Name: "notes"})
	for _, table := range []*Table[item, int64, customTenant]{insert, update} {
		src := FromTable(table, "i", Global()).source
		c, err := newTypedCompiler(true, src)
		require.NoError(t, err)
		meta, err := c.readRequirements()
		physical := src.table.sql()
		if table == insert {
			require.ErrorIs(t, err, ErrOperationDenied)
			meta = StatementShape{Effects: EffectRead, Access: []ShapeAccess{{physical, "SELECT"}}}
		} else {
			require.NoError(t, err)
		}
		meta.Result = []ShapeField{{Name: "count", Type: Int8().id.qualified()}}
		read, err := c.finishShape("read_items", "SELECT count(*) FROM "+physical, catalogType{}, meta)
		require.NoError(t, err)
		err = owner.registerShapes("read_items", []*compiledShape{read})
		if table == insert {
			require.ErrorIs(t, err, ErrOperationDenied)
			require.Empty(t, owner.Definition().StatementShapes)
			require.Nil(t, read.authority)
			continue
		}
		require.NoError(t, err)
		meta = StatementShape{Effects: EffectWrite, Access: []ShapeAccess{{physical, "UPDATE"}}}
		for _, col := range []string{"rank", "label"} {
			meta.Privileges = []DefinedPrivilege{{Table: physical, Privilege: "UPDATE", Columns: []string{col}}}
			key := "update_" + col
			shape, err := c.finishShape(key, "UPDATE "+physical+" SET "+typedQuote(col)+" = DEFAULT", catalogType{}, meta)
			require.NoError(t, err)
			err = owner.registerShapes(key, []*compiledShape{shape})
			if col == "label" {
				require.NoError(t, err)
				require.Contains(t, owner.Definition().Statements, key)
			} else {
				require.ErrorIs(t, err, ErrOperationDenied)
				require.NotContains(t, owner.Definition().Statements, key)
			}
		}
	}
}
