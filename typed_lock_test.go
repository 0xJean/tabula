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
	source := RowsFrom(table, src)
	projection := Project(Output(view, "id", Int8()), id)
	spec := RowsSpec{RowFilter: RowFilter{Where: Eq(id, slot.Expr())}}
	for strength, prefix := range map[RowLockStrength]string{ForUpdate: "UPDATE", ForNoKeyUpdate: "NO KEY UPDATE", ForShare: "SHARE", ForKeyShare: "KEY SHARE"} {
		for wait, suffix := range map[LockWait]string{Wait: "", SkipLocked: " SKIP LOCKED", NoWait: " NOWAIT"} {
			spec.Lock = ForRows(strength, src, wait)
			key := fmt.Sprintf("lock_%d_%d", strength, wait)
			p := DefineRows(view, key, source, spec, projection)
			shape, err := p.family.variant(false, nil, "data")
			require.NoError(t, err)
			require.Equal(t, `SELECT "i""d"."id" AS "id" FROM "public"."items" AS "i""d" WHERE ("i""d"."id" = CAST($1 AS "pg_catalog"."int8")) FOR `+prefix+` OF "i""d"`+suffix, shape.sql)
			require.Equal(t, []string{`"public"."items"`}, shape.shape.AnyColumnUpdate)
			require.Equal(t, EffectRowLock|EffectRead, shape.shape.Effects)
			require.Equal(t, []DefinedPrivilege{{Table: `"public"."items"`, Privilege: "SELECT", Columns: []string{"id"}}}, shape.shape.Privileges)
			require.Equal(t, []ShapeAccess{{`"public"."items"`, "SELECT"}}, shape.shape.Access)
			require.Equal(t, key, view.Definition().StatementQueries[shape.key])
		}
	}

	before := view.Definition()
	for _, lock := range []RowLock{ForRows(0, src, Wait), ForRows(ForUpdate, src, Try), ForRows(0, Rel[typedItem]{}, 0)} {
		spec.Lock = lock
		_, err := TryDefineRows(view, "invalid", source, spec, projection)
		require.Error(t, err)
	}
	another := FromTable(table, `i"d`, Global())
	spec.Lock = ForRows(ForUpdate, another, Wait)
	_, err := TryDefineRows(view, "invalid", source, spec, projection)
	require.ErrorContains(t, err, "outside")
	c, err := newTypedCompiler(false, src.source)
	require.NoError(t, err)
	_, err = spec.Lock.clause(c, new(StatementShape))
	require.ErrorContains(t, err, "outside")
	require.Equal(t, before, view.Definition())
	spec.Lock = ForRows(ForShare, src, Wait)
	_, err = TryDefineRows(view, "aggregate", source, spec, Project(Output(view, "id", Int8()), CountAll()))
	require.ErrorContains(t, err, "aggregate")
	spec.GroupBy = []RowColumn{FieldOf(src, "id", Int8()).Column()}
	_, err = TryDefineRows(view, "grouped", source, spec, projection)
	require.ErrorContains(t, err, "ungrouped")
	spec.GroupBy = nil
	spec.WithExists = true
	_, err = TryDefineRows(view, "summary", source, spec, projection)
	require.ErrorContains(t, err, "WithExists")
	spec.WithExists = false
	spec.Where = Input("flag", CatalogType[bool]("public", "choice", nil)).Expr()
	_, err = TryDefineRows(view, "not_boolean", source, spec, projection)
	require.ErrorContains(t, err, "pg_catalog.bool")
	require.Equal(t, before, view.Definition())

	for _, mode := range []TableSpec[typedItem, int64]{
		{Name: "public.jobs", RelationScope: Global(), ReadOnly: true},
		{Name: "public.notes", RelationScope: Global(), WriteMode: WriteInsertOnly},
	} {
		table := Define(reg, mode)
		src := FromTable(table, "i", Global())
		_, err := TryDefineRows(view, "denied", RowsFrom(table, src), RowsSpec{Lock: ForRows(ForShare, src, Wait)},
			Project(Output(view, "id", Int8()), FieldOf(src, "id", Int8()).Expr()))
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
	require.Panics(t, func() { RowsFrom(table, src) })
	source := RowsFrom(table, src, FieldOf(src, "group_id", CatalogType[customTenant]("pg_catalog", "text", nil)).Column())
	spec := RowsSpec{Lock: ForRows(ForUpdate, src, Wait), RowFilter: RowFilter{Where: Eq(id, slot.Expr())}}
	plan := DefineRows(view, "scoped", source, spec, Project(Output(view, "id", Int8()), id))
	def := view.Definition()
	require.Contains(t, def.Statements["scoped.scoped.data"], `("i"."group_id" = CAST($1 AS "pg_catalog"."text")) AND ("i"."status" = CAST($2 AS "pg_catalog"."text")) AND ("i"."id" = CAST($3 AS "pg_catalog"."int8"))`)
	require.Equal(t, []string{"group_id", "id", "status"}, def.StatementShapes["scoped.scoped.data"].Privileges[0].Columns)
	s := &routineSession{authority: reg.SQLAuthority(), method: CallEach}
	s.unscoped = true
	_, err := plan.Bind(s).Find(t.Context(), RowChoice{}, BindValue(slot, 1))
	require.ErrorContains(t, err, "unregistered")
	require.Zero(t, s.calls)
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
	spec := RowsSpec{Lock: ForRows(ForUpdate, rel, Wait)}
	p := DefineRows(view, "visible", RowsFrom(table, rel, status.Column()), spec, Project(Output(view, "id", Int8()), id.Expr()))
	def := view.Definition()
	shape := def.StatementShapes["visible.global.data"]
	require.Contains(t, def.Statements["visible.global.data"], `("i"."status" `+operator+" CAST($1 AS "+kind.id.qualified()+"))")
	require.Equal(t, []ShapeInput{{Name: "@live", Type: kind.id.qualified(), Secret: true}}, shape.Inputs)
	require.Contains(t, shape.PhysicalColumns, ShapeColumn{Relation: `"public"."items"`, Name: "status", Type: kind.id.qualified()})
	require.Contains(t, shape.SchemaUsage, kind.id.schema)
	s := &routineSession{authority: reg.SQLAuthority(), method: CallEach}
	_, err := p.Bind(s).Find(t.Context(), RowChoice{})
	require.NoError(t, err)
	require.Equal(t, []any{want}, s.lastArgs)
	_, err = p.Bind(s).Find(t.Context(), RowChoice{}, BindValue(Input("@live", kind), *new(V)))
	require.Error(t, err)
	require.Equal(t, 1, s.calls)
	require.Panics(t, func() { RowsFrom(table, rel, id.Column()) })
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
	DefineRows(view, "visible", RowsFrom(table, rel), RowsSpec{Lock: ForRows(ForShare, rel, Wait)},
		Project(Output(view, "id", Int8()), id))
	def := view.Definition()
	const key = "visible.global.data"
	require.Contains(t, def.Statements[key], `("i"."removed" IS NULL)`)
	require.Empty(t, def.StatementShapes[key].Inputs)
	require.Contains(t, def.StatementShapes[key].PhysicalColumns, ShapeColumn{Relation: `"public"."items"`, Name: "removed", Type: Timestamptz().id.qualified()})
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
