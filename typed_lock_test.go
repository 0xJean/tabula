package tabula

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type lockFlag struct {
	Held bool `db:"held"`
}

func TestTypedRowLockModes(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	view := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	table := Define(reg, TableSpec[typedItem, int64]{Name: "public.items", RelationScope: Global()})
	src := FromTable(table, `i"d`, Global())
	id := FieldOf(src, "id", Int8()).Expr()
	slot := Input("id", Int8())
	spec := LockedRowsSpec[customTenant]{Where: Eq(id, slot.Expr()), Select: []Projection{Project(Output(view, "id", Int8()), id)}}
	for strength, prefix := range map[RowLockStrength]string{ForUpdate: "UPDATE", ForNoKeyUpdate: "NO KEY UPDATE", ForShare: "SHARE", ForKeyShare: "KEY SHARE"} {
		for wait, suffix := range map[LockWait]string{Wait: "", SkipLocked: " SKIP LOCKED", NoWait: " NOWAIT"} {
			spec.Lock = ForRows(strength, src, wait)
			key := fmt.Sprintf("lock_%d_%d", strength, wait)
			p := DefineLockedRows(view, key, table, spec)
			shape := p.query.shape
			require.Equal(t, `SELECT "i""d"."id" AS "id" FROM "public"."items" AS "i""d" WHERE ("i""d"."id" = CAST($1 AS "pg_catalog"."int8")) FOR `+prefix+` OF "i""d"`+suffix, shape.sql)
			require.Equal(t, []string{`"public"."items"`}, shape.shape.AnyColumnUpdate)
			require.Equal(t, EffectRowLock|EffectRead, shape.shape.Effects)
			require.Equal(t, []DefinedPrivilege{{Table: `"public"."items"`, Privilege: "SELECT", Columns: []string{"id"}}}, shape.shape.Privileges)
			require.Equal(t, []ShapeAccess{{`"public"."items"`, "SELECT"}}, shape.shape.Access)
			require.Equal(t, key, view.Definition().StatementQueries[shape.key])
		}
	}

	before := view.Definition()
	for _, lock := range []RowLock{ForRows(0, src, Wait), ForRows(ForUpdate, src, Try), {}} {
		spec.Lock = lock
		_, err := TryDefineLockedRows(view, "invalid", table, spec)
		require.Error(t, err)
	}
	another := FromTable(table, `i"d`, Global())
	spec.Lock = ForRows(ForUpdate, another, Wait)
	_, err := TryDefineLockedRows(view, "invalid", table, spec)
	require.ErrorContains(t, err, "outside")
	c, err := newTypedCompiler(false, src.source)
	require.NoError(t, err)
	_, err = spec.Lock.clause(c, new(StatementShape))
	require.ErrorContains(t, err, "outside")
	require.Equal(t, before, view.Definition())
	spec.Lock = ForRows(ForShare, src, Wait)
	spec.Where = Input("flag", CatalogType[bool]("public", "choice", nil)).Expr()
	_, err = TryDefineLockedRows(view, "not_boolean", table, spec)
	require.ErrorContains(t, err, "PostgreSQL boolean")

	for _, mode := range []TableSpec[typedItem, int64]{
		{Name: "public.jobs", RelationScope: Global(), ReadOnly: true},
		{Name: "public.notes", RelationScope: Global(), WriteMode: WriteInsertOnly},
	} {
		table := Define(reg, mode)
		src := FromTable(table, "i", Global())
		_, err := TryDefineLockedRows(view, "denied", table, LockedRowsSpec[customTenant]{
			Lock: ForRows(ForShare, src, Wait), Select: []Projection{Project(Output(view, "id", Int8()), FieldOf(src, "id", Int8()).Expr())},
		})
		if mode.ReadOnly {
			require.ErrorIs(t, err, ErrReadOnly)
		} else {
			require.ErrorIs(t, err, ErrOperationDenied)
		}
	}
}

func TestTypedRowLockScopeAndVisibility(t *testing.T) {
	type scoped struct {
		ID     int64        `db:"id,key"`
		Group  customTenant `db:"group_id,scope"`
		Status string       `db:"status,deleted"`
	}
	reg := NewRegistry(RegistryConfig[customTenant]{})
	view := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	table := Define(reg, TableSpec[scoped, int64]{Name: "public.items", SoftDelete: &SoftDelete{DeletedValue: "removed", AliveValue: `open'\entry`}})
	src := FromTable(table, "i", ByScopeColumn())
	id := FieldOf(src, "id", Int8()).Expr()
	slot := Input("id", Int8())
	spec := LockedRowsSpec[customTenant]{Lock: ForRows(ForUpdate, src, Wait), Where: Eq(id, slot.Expr()), Select: []Projection{Project(Output(view, "id", Int8()), id)}}
	_, err := TryDefineLockedRows(view, "scoped", table, spec)
	require.ErrorContains(t, err, "scope column")
	spec.ScopeType = CatalogType[customTenant]("pg_catalog", "text", nil)
	plan := DefineLockedRows(view, "scoped", table, spec)
	require.Contains(t, plan.query.shape.sql, `("i"."group_id" = CAST($1 AS "pg_catalog"."text")) AND ("i"."status" = CAST($2 AS "pg_catalog"."text")) AND ("i"."id" = CAST($3 AS "pg_catalog"."int8"))`)
	require.Equal(t, []string{"group_id", "id", "status"}, plan.query.shape.shape.Privileges[0].Columns)
	_, err = plan.All(t.Context(), &fakeSession{unscoped: true}, BindValue(slot, 1))
	require.ErrorIs(t, err, ErrStatementAuthority)
}

func lockStatusSnapshot[V any](t *testing.T, kind PGType[V], deleted, alive any, operator string, want any) ViewDefinition {
	t.Helper()
	type record struct {
		ID     int64 `db:"id,key"`
		Status V     `db:"status,deleted"`
	}
	reg := NewRegistry(RegistryConfig[customTenant]{})
	view := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	table := Define(reg, TableSpec[record, int64]{Name: "public.items", RelationScope: Global(), SoftDelete: &SoftDelete{DeletedValue: deleted, AliveValue: alive}})
	rel := FromTable(table, "i", Global())
	id := FieldOf(rel, "id", Int8())
	status := FieldOf(rel, "status", kind)
	spec := LockedRowsSpec[customTenant]{Lock: ForRows(ForUpdate, rel, Wait), DeletedField: status, Select: []Projection{Project(Output(view, "id", Int8()), id.Expr())}}
	p := DefineLockedRows(view, "visible", table, spec)
	def := view.Definition()
	shape := def.StatementShapes["visible.lock"]
	require.Contains(t, def.Statements["visible.lock"], `("i"."status" `+operator+" CAST($1 AS "+kind.id.qualified()+"))")
	require.Equal(t, []ShapeInput{{Name: "@live", Type: kind.id.qualified(), Secret: true}}, shape.Inputs)
	require.Contains(t, shape.PhysicalColumns, ShapeColumn{Relation: `"public"."items"`, Name: "status", Type: kind.id.qualified()})
	require.Contains(t, shape.SchemaUsage, kind.id.schema)
	s := &routineSession{authority: reg.SQLAuthority(), method: CallEach}
	_, err := p.All(t.Context(), s)
	require.NoError(t, err)
	require.Equal(t, []any{want}, s.lastArgs)
	_, err = p.All(t.Context(), s, BindValue(Input("@live", kind), *new(V)))
	require.Error(t, err)
	require.Equal(t, 1, s.calls)
	spec.DeletedField = id
	_, err = TryDefineLockedRows(view, "wrong", table, spec)
	require.ErrorContains(t, err, "DeletedField")
	return def
}

func TestTypedLockBindsSoftDeleteStatus(t *testing.T) {
	type state string
	kind := CatalogType[state]("public", "state", func(s state) any { return string(s) })
	first := lockStatusSnapshot(t, kind, state("closed"), state(`one'\value`), "=", `one'\value`)
	second := lockStatusSnapshot(t, kind, state("closed"), state("two"), "=", "two")
	require.Equal(t, first, second)
	first = lockStatusSnapshot(t, Int4(), int32(3), nil, "<>", int32(3))
	second = lockStatusSnapshot(t, Int4(), int32(7), nil, "<>", int32(7))
	require.Equal(t, first, second)
}

func TestTypedLockTimestampVisibility(t *testing.T) {
	type record struct {
		ID      int64      `db:"id,key"`
		Removed *time.Time `db:"removed,deleted"`
	}
	reg := NewRegistry(RegistryConfig[customTenant]{})
	view := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	table := Define(reg, TableSpec[record, int64]{Name: "public.items", RelationScope: Global()})
	rel := FromTable(table, "i", Global())
	id := FieldOf(rel, "id", Int8()).Expr()
	p := DefineLockedRows(view, "visible", table, LockedRowsSpec[customTenant]{Lock: ForRows(ForShare, rel, Wait), Select: []Projection{Project(Output(view, "id", Int8()), id)}})
	require.Contains(t, p.query.shape.sql, `("i"."removed" IS NULL)`)
	require.Empty(t, p.query.shape.shape.Inputs)
	require.Contains(t, p.query.shape.shape.PhysicalColumns, ShapeColumn{Relation: `"public"."items"`, Name: "removed", Type: Timestamptz().id.qualified()})
}

func TestTypedAdvisoryOverloads(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	view := DefineView(reg, ViewSpec[lockFlag]{Name: "entries"})
	big, a, b := Input("key", Int8()), Input("first", Int4()), Input("second", Int4())
	for _, mode := range []AdvisoryMode{Exclusive, Shared} {
		for _, wait := range []LockWait{Wait, Try} {
			locks := []AdvisoryLock{
				AdvisoryXactLock(mode, wait, big.Expr()),
				AdvisoryXactLockPair(mode, wait, a.Expr(), b.Expr()),
			}
			for i, lock := range locks {
				name := "pg_advisory_xact_lock"
				if wait == Try {
					name = "pg_try_advisory_xact_lock"
				}
				if mode == Shared {
					name += "_shared"
				}
				args := `"pg_catalog"."int8"`
				if i == 1 {
					args = `"pg_catalog"."int4", "pg_catalog"."int4"`
				}
				sig := `"pg_catalog"."` + name + `"(` + args + `)`
				p := DefineAdvisoryLock(view, fmt.Sprintf("lock_%d_%d_%d", mode, wait, i), lock)
				meta := p.query.shape.shape
				require.Equal(t, EffectRead|EffectAdvisoryLock, meta.Effects)
				require.Equal(t, []DefinedPrivilege{{Function: sig, Privilege: "EXECUTE"}}, meta.Privileges)
				require.Len(t, meta.Inputs, i+1)
				require.Contains(t, p.query.shape.sql, `"`+name+`"(CAST($1 AS "pg_catalog".`)
				require.Equal(t, wait == Wait, strings.Contains(p.query.shape.sql, " FROM "))
			}
		}
	}
	domain := CatalogType[int64]("public", "key", nil)
	nullable := CatalogType[int64]("pg_catalog", "int8", func(v int64) any { return v })
	for _, lock := range []AdvisoryLock{
		{}, AdvisoryXactLock(0, Try, big.Expr()), AdvisoryXactLock(Shared, NoWait, big.Expr()),
		AdvisoryXactLock(Shared, Wait, Input("key", domain).Expr()), AdvisoryXactLock(Shared, Wait, Input("key", nullable).Expr()),
	} {
		_, err := TryDefineAdvisoryLock(view, "invalid", lock)
		require.Error(t, err)
	}
}
