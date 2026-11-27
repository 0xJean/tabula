package plan

import (
	"github.com/0xJean/tabula"

	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type statementItem struct {
	ID    int64  `db:"id,key"`
	Rank  int32  `db:"rank"`
	Label string `db:"label"`
	Once  string `db:"code,once"`
}

func statementSource[S any](reg *tabula.Registry[S], mode tabula.WriteMode) (*tabula.Table[statementItem, int64, S], RowSource[statementItem, S]) {
	table := tabula.DefinePlanSource(reg, tabula.TableSpec[statementItem, int64]{Name: "public.items", RelationScope: tabula.Global(), WriteMode: mode})
	return table, MapTable(table).As("i")
}

func TestWriteInsertAndTargetGuards(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table, src := statementSource(reg, tabula.WriteInsertOnly)
	id, rank := FieldOf(src.Rel(), "id", Int8()), FieldOf(src.Rel(), "rank", Int4())
	key, value := Input("key", Int8()), Input("value", Int4())
	insert := InsertInto(src).Values(Assign(id, key.Expr()), Assign(rank, value.Expr())).DoNothing()
	plan, err := TryDefineWrite(src, "append", insert)
	require.NoError(t, err)
	shape := table.Definition().StatementShapes["append.global.write"]
	require.Equal(t, []tabula.ShapeAccess{{Relation: `"public"."items"`, Command: "INSERT"}}, shape.Access)
	require.Equal(t, []tabula.DefinedPrivilege{{Table: `"public"."items"`, Privilege: "INSERT", Columns: []string{"id", "rank"}}}, shape.Privileges)
	require.Equal(t, []string{"label", "code"}, shape.Defaults[0].Columns)
	require.Len(t, shape.Inputs, 2)
	require.NotNil(t, plan)
	before := table.Definition()
	for _, query := range []WriteStatement{
		InsertInto(src), InsertInto(src).Values(), InsertInto(src).DefaultValues().Values(Assign(id, key.Expr())),
		InsertInto(src).Values(Assign(id, key.Expr()), Assign(id, key.Expr())),
		InsertInto(src).Values(Assign(id, key.Expr())).Values(Assign(rank, value.Expr())),
		InsertInto(src).Values(Assign(id, id.Expr())),
		InsertInto(src).Values(Assign(id, key.Expr())).OnConflict(ConflictColumns(id.Column())),
		InsertInto(src).Values(Assign(id, key.Expr())).OnConflict(ConflictTarget{}).DoNothing(),
		insert.OnConflict(ConflictColumns(id.Column())).DoUpdate(Assign(rank, value.Expr())),
		UpdateOf(src).Set(Assign(rank, value.Expr())).AllRows(), DeleteFrom(src).AllRows(),
	} {
		_, err := TryDefineWrite(src, "invalid", query)
		require.Error(t, err)
		require.Equal(t, before, table.Definition())
	}
	out := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "entries"})
	_, err = TryDefineReturning(out, "invalid", WithReturning(insert, Project(Output(out, "id", Int8()), id.Expr())))
	require.ErrorIs(t, err, tabula.ErrOperationDenied)
	require.Empty(t, out.Definition().Statements)
}

func TestWriteUpdateMinimumColumns(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table, src := statementSource(reg, tabula.WriteUpdateOnly)
	id, rank := FieldOf(src.Rel(), "id", Int8()), FieldOf(src.Rel(), "rank", Int4())
	key, value := Input("key", Int8()), Input("value", Int4())
	query := UpdateOf(src).Set(Assign(rank, value.Expr())).Where(Eq(id.Expr(), key.Expr()))
	_, err := TryDefineWrite(src, "change", query)
	require.NoError(t, err)
	shape := table.Definition().StatementShapes["change.global.write"]
	for _, privilege := range shape.Privileges {
		if privilege.Privilege == "SELECT" {
			require.Equal(t, []string{"id"}, privilege.Columns)
		}
		if privilege.Privilege == "UPDATE" {
			require.Equal(t, []string{"rank"}, privilege.Columns)
		}
	}
	for _, query := range []WriteStatement{
		UpdateOf(src).Set(Assign(rank, value.Expr())),
		UpdateOf(src).Set(Assign(id, key.Expr())).AllRows(),
		UpdateOf(src).Set(Assign(FieldOf(src.Rel(), "code", Text()), Input("code", Text()).Expr())).AllRows(),
		UpdateOf(src).Set(Assign(rank, value.Expr()), Assign(rank, value.Expr())).AllRows(),
		InsertInto(src).DefaultValues(), DeleteFrom(src).AllRows(),
	} {
		_, err := TryDefineWrite(src, "invalid", query)
		require.Error(t, err)
	}
	_, err = TryDefineWrite(src, "reset", UpdateOf(src).Set(DefaultOf(rank)).AllRows())
	require.NoError(t, err)
	shape = table.Definition().StatementShapes["reset.global.write"]
	require.Equal(t, []tabula.DefaultRequirement{{Relation: `"public"."items"`, Columns: []string{"rank"}}}, shape.Defaults)
	require.Equal(t, []tabula.ShapeAccess{{Relation: `"public"."items"`, Command: "UPDATE"}}, shape.Access)
}

func TestWriteScopeAndVersion(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[customTenant]{})
	rel, src := scopedRowSource(reg, tabula.TableSpec[rowPlanItem, int64]{Name: "public.items"})
	id, rank, scope := FieldOf(rel, "id", Int8()), FieldOf(rel, "rank", Int4()), FieldOf(rel, "scope_id", tabula.CatalogType[customTenant]("pg_catalog", "text", nil))
	key, value, scopeInput := Input("id", Int8()), Input("value", Int4()), Input("scope", scope.kind)
	insert := InsertInto(src).Values(Assign(id, key.Expr()), Assign(rank, value.Expr()))
	_, err := TryDefineWrite(src, "append", insert)
	require.NoError(t, err)
	shape := tableDef(reg, "public.items").StatementShapes["append.scoped.write"]
	require.True(t, shape.Inputs[0].Scope)
	require.Len(t, shape.Access, 1)
	require.Contains(t, shape.Privileges[0].Columns, "scope_id")
	_, err = TryDefineWrite(src, "bad_scope", InsertInto(src).Values(Assign(scope, scopeInput.Expr())))
	require.Error(t, err)
	_, err = TryDefineWrite(src, "missing_scope", insert.UnscopedOnly())
	require.Error(t, err)
	_, err = TryDefineWrite(src, "explicit_scope", InsertInto(src).Values(Assign(id, key.Expr()), Assign(scope, scopeInput.Expr())).UnscopedOnly())
	require.NoError(t, err)
	require.True(t, tableDef(reg, "public.items").StatementShapes["explicit_scope.unscoped.write"].UnscopedOnly)
	_, err = TryDefineWrite(src, "missing_version", UpdateOf(src).Set(Assign(rank, value.Expr())).AllRows())
	require.ErrorIs(t, err, tabula.ErrOperationDenied)
	revision := FieldOf(rel, "revision", Int4())
	_, err = TryDefineWrite(src, "versioned", UpdateOf(src).Set(Assign(rank, value.Expr())).AllRows().Version(ExpectVersion(revision, value)))
	require.NoError(t, err)
	text := tableDef(reg, "public.items").Statements["versioned.scoped.write"]
	require.Contains(t, text, `"revision" = "_tabula_0"."revision" OPERATOR(pg_catalog.+) 1`)
	require.Contains(t, text, `"removed" OPERATOR(pg_catalog.=) FALSE`)
}

func TestWriteIndexPredicatesAndExcluded(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table, src := statementSource(reg, tabula.WriteDefault)
	id, rank, label := FieldOf(src.Rel(), "id", Int8()), FieldOf(src.Rel(), "rank", Int4()), FieldOf(src.Rel(), "label", Text())
	key, value, text := Input("id", Int8()), Input("rank", Int4()), Input("label", Text())
	index := ConflictColumns(id.Column()).WhereIndex(IndexAnd(IndexGt(rank, int32(1)), IndexEq(label, "'\\; --")))
	insert := InsertInto(src).Values(Assign(id, key.Expr()), Assign(rank, value.Expr()), Assign(label, text.Expr())).
		OnConflict(index).DoUpdate(Assign(rank, Excluded(rank))).UpdateWhere(Ne(rank.Expr(), Excluded(rank)))
	_, err := TryDefineWrite(src, "put", insert)
	require.NoError(t, err)
	query := table.Definition().Statements["put.global.write"]
	require.Contains(t, query, `CAST(E'''\\; --' AS "pg_catalog"."text")`)
	require.Contains(t, query, `"rank" = excluded."rank"`)
	require.Len(t, table.Definition().StatementShapes["put.global.write"].Inputs, 3)
	require.True(t, table.Definition().StatementShapes["put.global.write"].Conflicts[0].DoUpdate)
	_, err = TryDefineWrite(src, "wrong_phase", UpdateOf(src).Set(Assign(rank, Excluded(rank))).AllRows())
	require.ErrorContains(t, err, "EXCLUDED")
	_, err = TryDefineWrite(src, "protected", insert.DoUpdate(Assign(id, Excluded(id))))
	require.ErrorIs(t, err, tabula.ErrOperationDenied)
	_, err = TryDefineWrite(src, "capture_excluded", insert.DoUpdate(Assign(rank,
		Coalesce(ScalarSubquery(SelectValue(Excluded(rank))), value.Expr()))))
	require.Error(t, err)
	_, err = TryDefineWrite(src, "outer_excluded", insert.DoUpdate(Assign(rank,
		Coalesce(ScalarSubquery(SelectValue(Outer(Excluded(rank)))), value.Expr()))))
	require.NoError(t, err)
	_, err = TryDefineWrite(src, "nothing", insert.DoUpdate(Assign(rank,
		Coalesce(ScalarSubquery(SelectValue(Excluded(rank))), value.Expr()))).DoNothing())
	// UpdateWhere remains an explicit invalid phase when DoNothing is selected.
	require.ErrorContains(t, err, "UpdateWhere")
	_, err = TryDefineWrite(src, "empty_predicate", InsertInto(src).Values(Assign(id, key.Expr())).
		OnConflict(ConflictColumns(id.Column()).WhereIndex(IndexPredicate{})).DoNothing())
	require.ErrorContains(t, err, "undeclared index predicate")
	notes := MapTable(tabula.DefinePlanSource(reg, tabula.TableSpec[statementItem, int64]{Name: "public.notes", RelationScope: tabula.Global()})).As("n")
	oldValue := Coalesce(ScalarSubquery(SelectValue(FieldOf(notes.Rel(), "rank", Int4()).Expr()).From(notes)), value.Expr())
	_, err = TryDefineWrite(src, "nothing_replaced", InsertInto(src).Values(Assign(id, key.Expr())).
		OnConflict(ConflictColumns(id.Column())).DoUpdate(Assign(rank, oldValue)).DoNothing())
	require.NoError(t, err)
	shape := table.Definition().StatementShapes["nothing_replaced.global.write"]
	require.Len(t, shape.Inputs, 1)
	for _, access := range shape.Access {
		require.NotEqual(t, `"public"."notes"`, access.Relation)
	}
	for _, predicate := range []IndexPredicate{{}, IndexEq(label, "\x00"), IndexAnd(IndexGt(rank, int32(1)), IndexPredicate{})} {
		if predicate.node == nil {
			predicate = IndexNot(predicate)
		}
		_, err := TryDefineWrite(src, "invalid_predicate", InsertInto(src).Values(Assign(id, key.Expr())).OnConflict(ConflictColumns(id.Column()).WhereIndex(predicate)).DoNothing())
		require.Error(t, err)
	}
	var secret = *label.ref
	secret.secret = true
	_, err = TryDefineWrite(src, "secret", InsertInto(src).Values(Assign(id, key.Expr())).OnConflict(
		ConflictColumns(id.Column()).WhereIndex(IndexEq(Field[string]{ref: &secret, kind: Text()}, "private"))).DoNothing())
	require.ErrorContains(t, err, "non-secret")
	_, err = TryDefineWrite(src, "constraint", InsertInto(src).Values(Assign(id, key.Expr())).OnConflict(ConflictConstraint(`items"key`)).DoNothing())
	require.NoError(t, err)
	require.Contains(t, table.Definition().Statements["constraint.global.write"], `ON CONSTRAINT "items""key"`)
}

func TestWriteReturningCTEAndMatches(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	table, src := statementSource(reg, tabula.WriteDefault)
	id, rank := FieldOf(src.Rel(), "id", Int8()), FieldOf(src.Rel(), "rank", Int4())
	value := Input("value", Int4())
	out := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "entries"})
	read := Select(Project(Output(out, "id", Int8()), id.Expr())).From(src)
	_, err := TryDefineReturning(out, "read_as_write", read)
	require.Error(t, err)
	changed := WriteCTE("changed", WithReturning(UpdateOf(src).Set(Assign(rank, value.Expr())).AllRows(), Export("id", id.Expr()).Project()))
	_, err = TryDefineReturning(out, "duplicate", read.With(changed, WriteCTE("again", WithReturning(UpdateOf(src).Set(DefaultOf(rank)).AllRows(), Export("id", id.Expr()).Project()))))
	require.ErrorContains(t, err, "multiple writes")
	require.Empty(t, table.Definition().Statements)
	require.Empty(t, out.Definition().Statements)
	same := WithReturning(UpdateOf(src).Set(DefaultOf(rank)).AllRows(), Export("id", id.Expr()).Project())
	_, err = TryDefineReturning(out, "same_builder_twice", read.With(WriteCTE("first", same), WriteCTE("second", same)))
	require.ErrorContains(t, err, "multiple writes")
	_, err = TryDefineReturning(out, "changed", read.With(changed))
	require.NoError(t, err)
	require.True(t, out.Definition().StatementShapes["changed.global.write"].Effects&tabula.EffectWrite != 0)
	other := MapTable(table).As("n")
	otherID, otherRank := FieldOf(other.Rel(), "id", Int8()), FieldOf(other.Rel(), "rank", Int4())
	badCapture := ScalarSubquery(SelectValue(otherRank.Expr()).From(other).Where(Eq(otherID.Expr(), Outer(id.Expr()))))
	_, err = TryDefineWrite(src, "insert_capture", InsertInto(src).Values(Assign(rank, Coalesce(badCapture, value.Expr()))))
	require.Error(t, err)
	_, err = TryDefineWrite(src, "matched", UpdateOf(src).Set(Assign(rank, otherRank.Expr())).From(other, Eq(otherID.Expr(), id.Expr())).AllRows())
	require.NoError(t, err)
	require.Equal(t, []tabula.UniqueMatchRequirement{{Relation: `"public"."items"`, Columns: []string{"id"}}}, table.Definition().StatementShapes["matched.global.write"].UniqueMatches)
	_, err = TryDefineWrite(src, "many", UpdateOf(src).Set(Assign(rank, otherRank.Expr())).From(other, Gt(otherID.Expr(), id.Expr())).AllRows())
	require.ErrorContains(t, err, "unique key")
	for _, sql := range table.Definition().Statements {
		require.False(t, strings.Contains(sql, "LIMIT 1"))
	}
}

func TestWriteNestedLocksAndOptionalSources(t *testing.T) {
	reg := tabula.NewRegistry(tabula.RegistryConfig[customTenant]{})
	table, src := statementSource(reg, tabula.WriteDefault)
	id, rank := FieldOf(src.Rel(), "id", Int8()), FieldOf(src.Rel(), "rank", Int4())
	notes := tabula.DefinePlanSource(reg, tabula.TableSpec[statementItem, int64]{Name: "public.notes", RelationScope: tabula.Global()})
	n := MapTable(notes).As("n")
	nid := FieldOf(n.Rel(), "id", Int8())
	plan := DefineWrite(src, "locked_input", InsertInto(src).Columns(id.Column()).FromSelect(
		Select(Export("id", nid.Expr()).Project()).From(n).Lock(ForRows(ForUpdate, n.Rel(), Wait))))
	shape := table.Definition().StatementShapes["locked_input.global.write"]
	require.Equal(t, tabula.EffectWrite|tabula.EffectRead|tabula.EffectRowLock, shape.Effects)
	require.Equal(t, []string{`"public"."notes"`}, shape.AnyColumnUpdate)
	compiled, err := plan.family.variant(false, nil, "write")
	require.NoError(t, err)
	require.Equal(t, uint8(shape.Effects), compiled.compiled.WriteEffects.Bits)
	_, scoped := scopedRowSource(reg, tabula.TableSpec[rowPlanItem, int64]{Name: "public.jobs"})
	jobID := FieldOf(scoped.Rel(), "id", Int8())
	key, value := Input("id", Int8()), Input("rank", Int4())
	out := tabula.DefineView(reg, tabula.ViewSpec[typedResult]{Name: "entries"})
	_, err = TryDefineUpdateReturning(src, out, "optional", MutationSpec{RowFilter: RowFilter{
		Where: Eq(id.Expr(), key.Expr()), Optional: []RowPredicate{{Name: "exists", When: ExistsQuery(Select(Export("id", jobID.Expr()).Project()).From(scoped))}},
	}}, []RowSet{{Name: "rank", Assignments: []RowAssignment{Assign(rank, value.Expr())}}}, Project(Output(out, "id", Int8()), id.Expr()))
	require.NoError(t, err)
	for key, shape := range out.Definition().StatementShapes {
		if strings.Contains(key, ".where.") {
			require.True(t, shape.Inputs[0].Scope)
		} else {
			for _, access := range shape.Access {
				require.NotEqual(t, `"public"."jobs"`, access.Relation)
			}
			for _, input := range shape.Inputs {
				require.False(t, input.Scope)
			}
		}
	}
}
