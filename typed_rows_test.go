package tabula

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

type rowPlanItem struct {
	ID      int64        `db:"id,key"`
	Scope   customTenant `db:"scope_id,scope"`
	Label   *string      `db:"label,secret"`
	Rank    int32        `db:"rank"`
	Code    string       `db:"code,once"`
	Version int32        `db:"revision,version"`
	Removed bool         `db:"removed,deleted"`
}

type rowPlanResult struct {
	ID   int64 `db:"id"`
	Rank int32 `db:"rank"`
}

type rowPlanSession struct {
	fakeSession
	authority SQLAuthority
	calls     []AuthorizedSQL
	readOnly  bool
	failure   error
}

func (s *rowPlanSession) ReadOnly() bool { return s.readOnly }
func (s *rowPlanSession) receive(call AuthorizedStatement, method CallMethod) error {
	sql, err := s.authority.Open(s, method, call)
	if err != nil {
		return err
	}
	s.calls = append(s.calls, sql)
	return s.failure
}
func (s *rowPlanSession) ScanAuthorized(_ context.Context, call AuthorizedStatement, dest ...any) error {
	if err := s.receive(call, CallScan); err != nil {
		return err
	}
	*dest[0].(*int64) = 8
	return nil
}
func (s *rowPlanSession) EachAuthorized(_ context.Context, call AuthorizedStatement, visit func(func(...any) error) error) error {
	if err := s.receive(call, CallEach); err != nil {
		return err
	}
	return visit(func(dest ...any) error {
		*dest[0].(*int64), *dest[1].(*int32) = 4, 5
		return nil
	})
}
func (s *rowPlanSession) ExecAuthorized(_ context.Context, call AuthorizedStatement) (int64, error) {
	return s.execRows, s.receive(call, CallExec)
}

func scopedRowSource(reg *Registry[customTenant], spec TableSpec[rowPlanItem, int64]) (Rel[rowPlanItem], RowSource[rowPlanItem, customTenant]) {
	table := Define(reg, spec)
	rel := FromTable(table, "i", ByScopeColumn())
	scope := FieldOf(rel, "scope_id", CatalogType[customTenant]("pg_catalog", "text", nil))
	return rel, RowsFrom(table, rel, scope.Column())
}

func TestRowsPlanBindingAndObservation(t *testing.T) {
	tracer := new(fakeTracer)
	reg := NewRegistry(RegistryConfig[customTenant]{Tracer: tracer})
	rel, src := scopedRowSource(reg, TableSpec[rowPlanItem, int64]{Name: "public.items"})
	id, rank := FieldOf(rel, "id", Int8()), FieldOf(rel, "rank", Int4())
	label := FieldOf(rel, "label", Text())
	view := DefineView(reg, ViewSpec[rowPlanResult]{Name: "entries"})
	minimum, projected := Input("minimum", Int8()), Input("projected", Int4())
	spec := RowsSpec{
		RowFilter: RowFilter{AllowUnscoped: true, Optional: []RowPredicate{{"match", Or(Ge(id.Expr(), minimum.Expr()), Eq(id.Expr(), minimum.Expr()))}, {"label", IsNull(label.Expr())}}},
		Orders:    []RowOrder{{"by_id", []RowTerm{Desc(id)}}},
		Pages:     RowPages{Unpaged: true, Offset: true, Keyset: true, MaxLimit: 10, MaxOffset: 20},
	}
	project := []Projection{Project(Output(view, "id", Int8()), id.Expr()), Project(Output(view, "rank", Int4()), projected.Expr())}
	p := DefineRows(view, "search", src, spec, project...)
	def := view.Definition()
	require.Len(t, def.StatementShapes, 2*4*(4+1))
	key := "search.scoped.data.sort.by_id.offset.where.match+label"
	count := "search.scoped.count.where.match+label"
	require.Equal(t, "search", def.StatementQueries[key])
	require.Equal(t, []ShapeInput{
		{Name: "scope", Type: Text().id.qualified(), Scope: true, Secret: true}, {Name: "projected", Type: Int4().id.qualified(), Secret: true},
		{Name: "minimum", Type: Int8().id.qualified(), Secret: true}, {Name: "@limit", Type: Int8().id.qualified(), Secret: true}, {Name: "@offset", Type: Int8().id.qualified(), Secret: true},
	}, def.StatementShapes[key].Inputs)
	require.Len(t, def.StatementShapes[count].Inputs, 2)
	require.NotContains(t, def.Statements[count], "ORDER BY")
	require.NotContains(t, def.Statements[count], "$3")
	require.Equal(t, []string{"id", "label", "removed", "scope_id"}, def.StatementShapes[key].Privileges[0].Columns)
	require.NotContains(t, def.Statements[key], `"rank" >`)
	require.Contains(t, def.Statements[key], `CAST($2 AS "pg_catalog"."int4") AS "rank"`)

	s := &rowPlanSession{fakeSession: fakeSession{tenant: "first"}, authority: reg.SQLAuthority()}
	b := p.Bind(s)
	ctx := t.Context()
	choice := RowChoice{Filters: []string{"label", "match"}, Order: "by_id", Page: OffsetPage, Limit: 5, Offset: 2}
	bindings := []Binding{BindValue(minimum, 1), BindValue(projected, 3)}
	rows, total, err := b.List(ctx, choice, bindings...)
	require.NoError(t, err)
	require.Equal(t, []rowPlanResult{{4, 5}}, rows)
	require.EqualValues(t, 8, total)
	require.Len(t, s.calls, 2)
	require.Equal(t, []any{customTenant("first"), int64(1)}, s.calls[0].Args)
	require.Equal(t, []any{customTenant("first"), int32(3), int64(1), int64(5), int64(2)}, s.calls[1].Args)
	require.Equal(t, "entries."+key, s.calls[1].Key)
	require.Equal(t, "entries."+count, tracer.spans[0].info.Key)
	require.Equal(t, s.calls[1].Key, tracer.spans[1].info.Key)

	other := DefineRows(view, "other", src, spec, project...)
	boundary, err := p.Cursor("by_id", At(id, 4))
	require.NoError(t, err)
	next := RowChoice{Order: "by_id", Page: AfterPage, Limit: 1, After: boundary}
	_, _, err = other.Bind(s).List(ctx, next, BindValue(projected, 2))
	require.ErrorContains(t, err, "different plan")
	_, err = p.Cursor("by_id", At(rank, 4))
	require.Error(t, err)
	_, err = p.Cursor("by_id", NullAt(id))
	require.Error(t, err)
	for _, c := range []RowChoice{
		{Order: "by_id", Page: AfterPage, Limit: 1}, {Order: "by_id", Page: FirstPage, Limit: 11},
		{Order: "by_id", Page: OffsetPage, Limit: 2, Offset: 21}, {Order: "by_id", Page: RowPage(99)},
		{Order: "by_id", Filters: []string{"unknown"}}, {Order: "by_id", Filters: []string{"label", "label"}},
		{Order: "by_id.offset"}, {Order: "by_id", After: boundary},
	} {
		_, _, err = b.List(ctx, c, BindValue(projected, 2))
		require.Error(t, err)
	}
	for _, args := range [][]Binding{nil, {BindValue(minimum, 1)}, {BindValue(projected, 1), BindValue(Input("minimum", Int8()), 1)},
		{BindValue(projected, 1), BindValue(minimum, 1), BindValue(minimum, 2)}} {
		_, _, err = b.List(ctx, choice, args...)
		require.Error(t, err)
	}
	require.Len(t, s.calls, 2)
	_, err = b.Find(ctx, next, BindValue(projected, 3))
	require.NoError(t, err)
	require.Equal(t, []any{customTenant("first"), int32(3), int64(4), int64(1)}, s.calls[2].Args)
	s.unscoped = true
	_, err = b.Count(ctx, nil)
	require.NoError(t, err)
	require.Contains(t, s.calls[3].Key, ".unscoped.")
	require.Empty(t, s.calls[3].Args)
	s.failure = errors.New("driver error")
	_, _, err = b.List(ctx, choice, bindings...)
	require.ErrorIs(t, err, s.failure)
	require.Len(t, s.calls, 5)
	_, err = p.Bind(&fakeSession{}).Count(ctx, nil)
	require.ErrorIs(t, err, ErrStatementAuthority)
}

func TestRowsPlanBudgetAndGrouping(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	rel, src := scopedRowSource(reg, TableSpec[rowPlanItem, int64]{Name: "public.items"})
	id, rank := FieldOf(rel, "id", Int8()), FieldOf(rel, "rank", Int4())
	type summary struct {
		Rank int32  `db:"rank"`
		Sum  *int64 `db:"sum"`
	}
	view := DefineView(reg, ViewSpec[summary]{Name: "entries"})
	cols := []Projection{Project(Output(view, "rank", Int4()), rank.Expr()), Project(Output(view, "sum", Int8()), SumOf(rank.Expr(), Int8()))}
	_, err := TryDefineRows(view, "grouped", src, RowsSpec{}, cols...)
	require.ErrorContains(t, err, "ungrouped")
	var optional []RowPredicate
	for i := range 8 {
		optional = append(optional, RowPredicate{fmt.Sprintf("option_%d", i), IsNull(id.Expr())})
	}
	spec := RowsSpec{RowFilter: RowFilter{Optional: optional}, GroupBy: []RowColumn{rank.Column()}}
	_, err = TryDefineRows(view, "grouped", src, spec, cols...)
	require.ErrorContains(t, err, "exceeds 256 shapes")
	require.ErrorContains(t, err, "filter.option_0=on")
	require.ErrorContains(t, err, "form=count")
	require.Empty(t, view.Definition().Statements)
	spec.Optional = nil
	_, err = TryDefineRows(view, "grouped", src, spec, cols...)
	require.NoError(t, err)
	d := view.Definition()
	require.Contains(t, d.Statements["grouped.scoped.count"], "GROUP BY")
	require.Contains(t, d.Statements["grouped.scoped.data"], `pg_catalog.sum("i"."rank")`)
	require.True(t, d.StatementShapes["grouped.scoped.data"].Result[1].Nullable)
	spec.Orders = []RowOrder{{"id", []RowTerm{Asc(id)}}}
	_, err = TryDefineRows(view, "invalid", src, spec, cols...)
	require.ErrorContains(t, err, "ungrouped order")
	spec.Orders = nil
	spec.Where = Eq(CountOf(id.Expr()), CountAll())
	_, err = TryDefineRows(view, "invalid", src, spec, cols...)
	require.ErrorContains(t, err, "aggregate outside")
}

func TestRowsPlanBooleanPredicates(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	rel, src := scopedRowSource(reg, TableSpec[rowPlanItem, int64]{Name: "public.items"})
	view := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	id := FieldOf(rel, "id", Int8())
	col := Project(Output(view, "id", Int8()), id.Expr())
	flag := Input("enabled", Bool()).Expr()
	text := CatalogType[bool]("pg_catalog", "text", nil)
	other := Input("flag", text).Expr()
	for _, pred := range []Pred{other, Or(flag, other), Not(other), And(flag, Or(flag, Not(other))), And(flag, other)} {
		_, err := TryDefineRows(view, "invalid", src, RowsSpec{RowFilter: RowFilter{Where: pred}}, col)
		require.ErrorContains(t, err, "pg_catalog.bool")
		_, err = TryDefineRows(view, "invalid", src, RowsSpec{RowFilter: RowFilter{Optional: []RowPredicate{{"flag", pred}}}}, col)
		require.ErrorContains(t, err, "pg_catalog.bool")
	}
	require.Empty(t, view.Definition().StatementShapes)
	DefineRows(view, "valid", src, RowsSpec{RowFilter: RowFilter{Where: Not(flag)}}, col)
	require.Contains(t, view.Definition().Statements["valid.scoped.data"], "(NOT ")
	require.Panics(t, func() {
		table := Define(reg, TableSpec[rowPlanItem, int64]{Name: "public.notes"})
		otherRel := FromTable(table, "n", ByScopeColumn())
		scope := FieldOf(otherRel, "scope_id", CatalogType[customTenant]("pg_catalog", "text", nil))
		RowsFrom(table, otherRel, scope.Column(), FieldOf(otherRel, "removed", text).Column())
	})
}

func TestRowsMutationRestrictions(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{Deny: DenyUnscopedWrites})
	rel, src := scopedRowSource(reg, TableSpec[rowPlanItem, int64]{Name: "public.items", WriteMode: WriteUpdateOnly, UpdateColumns: []string{"rank", "revision"}})
	key, rank := FieldOf(rel, "id", Int8()), FieldOf(rel, "rank", Int4())
	version := FieldOf(rel, "revision", Int4())
	input, minimum, expected := Input("rank", Int4()), Input("minimum", Int8()), Input("expected", Int4())
	spec := MutationSpec{RowFilter: RowFilter{Where: Ge(key.Expr(), minimum.Expr()), AllowUnscoped: true}, Version: ExpectVersion(version, expected)}
	update := func(a RowAssignment) error {
		_, err := TryDefineUpdateMany(src, "set_rank", spec, RowSet{"rank", []RowAssignment{a}})
		return err
	}
	code, group := FieldOf(rel, "code", Text()), FieldOf(rel, "scope_id", CatalogType[customTenant]("pg_catalog", "text", nil))
	for _, a := range []RowAssignment{
		Assign(key, minimum.Expr()), Assign(group, group.Expr()), Assign(code, code.Expr()), Assign(version, input.Expr()),
		Assign(FieldOf(rel, "removed", Bool()), Input("removed", Bool()).Expr()), {},
	} {
		require.ErrorIs(t, update(a), ErrOperationDenied)
	}
	set := RowSet{"rank", []RowAssignment{Assign(rank, input.Expr())}}
	_, err := TryDefineUpdateMany(src, "no_version", MutationSpec{RowFilter: spec.RowFilter}, set)
	require.ErrorIs(t, err, ErrOperationDenied)
	_, err = TryDefineUpdateMany(src, "no_where", MutationSpec{Version: spec.Version}, set)
	require.ErrorContains(t, err, "fixed typed predicate")
	_, err = TryDefineDeleteMany(src, "remove", spec)
	require.ErrorIs(t, err, ErrOperationDenied)
	p := DefineUpdateMany(src, "set_rank", spec, set)
	sh := src.owner.Definition().StatementShapes["set_rank.scoped.set.rank"]
	require.Equal(t, []DefinedPrivilege{
		{Table: src.rel.table.sql(), Privilege: "UPDATE", Columns: []string{"rank", "revision"}},
		{Table: src.rel.table.sql(), Privilege: "SELECT", Columns: []string{"id", "removed", "revision", "scope_id"}},
	}, sh.Privileges)
	s := &rowPlanSession{authority: reg.SQLAuthority(), readOnly: true}
	args := []Binding{BindValue(expected, 3), BindValue(input, 2), BindValue(minimum, 1)}
	b := p.Bind(s)
	_, err = b.UpdateMany(t.Context(), "rank", nil, args...)
	require.ErrorIs(t, err, ErrReadOnly)
	s.unscoped, s.readOnly = true, false
	_, err = b.UpdateMany(t.Context(), "rank", nil, args...)
	require.ErrorIs(t, err, ErrCapabilityDenied)
	s.unscoped = false
	_, err = b.DeleteMany(t.Context(), nil, args...)
	require.ErrorIs(t, err, ErrOperationDenied)
	_, err = b.UpdateMany(t.Context(), "other", nil, args...)
	require.Error(t, err)
	require.Empty(t, s.calls)
	s.execRows = 11
	n, err := b.UpdateMany(t.Context(), "rank", nil, args...)
	require.NoError(t, err)
	require.EqualValues(t, 11, n)
	require.Equal(t, []any{customTenant(""), int32(2), int64(1), int32(3)}, s.calls[0].Args)

	for _, spec := range []TableSpec[rowPlanItem, int64]{
		{Name: "public.jobs", WriteMode: WriteInsertOnly}, {Name: "public.jobs", ReadOnly: true},
	} {
		_, src := scopedRowSource(reg, spec)
		_, err := TryDefineUpdateMany(src, "change", MutationSpec{}, set)
		if spec.ReadOnly {
			require.ErrorIs(t, err, ErrReadOnly)
		} else {
			require.ErrorIs(t, err, ErrOperationDenied)
			view := DefineView(reg, ViewSpec[typedResult]{Name: "notes"})
			_, err = TryDefineRows(view, "read", src, RowsSpec{}, Project(Output(view, "id", Int8()), CountAll()))
			require.ErrorIs(t, err, ErrOperationDenied)
		}
	}
}

func TestRowsSourceStatusAndTypes(t *testing.T) {
	type state string
	type entry struct {
		ID     int64 `db:"id,key"`
		Status state `db:"status,deleted"`
	}
	reg := NewRegistry(RegistryConfig[customTenant]{})
	for _, alive := range []any{nil, state("active")} {
		tab := Define(reg, TableSpec[entry, int64]{Name: "public.items", RelationScope: Global(),
			SoftDelete: &SoftDelete{DeletedValue: state("closed"), AliveValue: alive}})
		rel := FromTable(tab, "i", Global())
		kind := CatalogType[state]("pg_catalog", "text", func(v state) any { return string(v) })
		src := RowsFrom(tab, rel, FieldOf(rel, "status", kind).Column())
		require.Panics(t, func() { RowsFrom(tab, rel) })
		require.Panics(t, func() {
			RowsFrom(tab, FromTable(Define(reg, TableSpec[entry, int64]{Name: "public.notes", RelationScope: Global(), SoftDelete: &SoftDelete{DeletedValue: state("closed")}}), "n", Global()))
		})
		id := FieldOf(rel, "id", Int8())
		view := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
		p := DefineRows(view, "status", src, RowsSpec{}, Project(Output(view, "id", Int8()), id.Expr()))
		s := &rowPlanSession{authority: reg.SQLAuthority()}
		_, err := p.Bind(s).Count(t.Context(), nil)
		require.NoError(t, err)
		q := s.calls[0]
		require.NotContains(t, q.Query, "closed")
		require.NotContains(t, q.Query, "active")
		if alive == nil {
			require.Contains(t, q.Query, " <> CAST($1 AS ")
			require.Equal(t, []any{"closed"}, q.Args)
		} else {
			require.Contains(t, q.Query, " = CAST($1 AS ")
			require.Equal(t, []any{"active"}, q.Args)
		}
	}
}

func TestRowsPlanRegistrationOrder(t *testing.T) {
	build := func(reverse bool) ViewDefinition {
		reg := NewRegistry(RegistryConfig[customTenant]{})
		rel, src := scopedRowSource(reg, TableSpec[rowPlanItem, int64]{Name: "public.items"})
		out := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
		id := FieldOf(rel, "id", Int8())
		slot := Input("id", Int8())
		spec := RowsSpec{RowFilter: RowFilter{Optional: []RowPredicate{{Name: "id", When: Eq(id.Expr(), slot.Expr())}}, AllowUnscoped: true},
			Orders: []RowOrder{{"later", []RowTerm{Desc(id)}}, {"earlier", []RowTerm{Asc(id)}}},
			Pages:  RowPages{Offset: true, Keyset: true, MaxLimit: 20}}
		names := []string{"read_items", "read_entries"}
		if reverse {
			names[0], names[1] = names[1], names[0]
			spec.Orders[0], spec.Orders[1] = spec.Orders[1], spec.Orders[0]
		}
		for _, name := range names {
			DefineRows(out, name, src, spec, Project(Output(out, "id", Int8()), id.Expr()))
		}
		return out.Definition()
	}
	require.Equal(t, build(false), build(true))
}

func TestRowsMutationTargetModifier(t *testing.T) {
	type entry struct {
		ID    int64  `db:"id,key"`
		Label string `db:"label"`
	}
	reg := NewRegistry(RegistryConfig[customTenant]{})
	tab := Define(reg, TableSpec[entry, int64]{Name: "public.items", RelationScope: Global()})
	rel := FromTable(tab, "i", Global())
	src := RowsFrom(tab, rel)
	label := FieldOf(rel, "label", Varchar(4))
	other := FieldOf(rel, "label", Varchar(5))
	id := FieldOf(rel, "id", Int8())
	spec := MutationSpec{RowFilter: RowFilter{Where: Eq(id.Expr(), Input("id", Int8()).Expr())}}
	set := func(value Expr[string]) RowSet {
		return RowSet{Name: "label", Assignments: []RowAssignment{Assign(label, value)}}
	}
	_, err := TryDefineUpdateMany(src, "conflict", spec, set(other.Expr()))
	require.Error(t, err)
	DefineUpdateMany(src, "change", spec, set(Input("next", Varchar(4)).Expr()))
	shape := tab.Definition().StatementShapes["change.global.set.label"]
	require.Contains(t, shape.PhysicalColumns, ShapeColumn{Relation: src.rel.table.sql(), Name: "label", Type: Varchar(4).id.qualified(), TypeMod: new(int32(8))})
	require.Contains(t, shape.Privileges, DefinedPrivilege{Table: src.rel.table.sql(), Privilege: "SELECT", Columns: []string{"id"}})
}
