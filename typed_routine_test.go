package tabula

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type routineSession struct {
	fakeSession
	authority SQLAuthority
	method    CallMethod
	readOnly  bool
	calls     int
}

func (s *routineSession) ReadOnly() bool { return s.readOnly }
func (s *routineSession) ExecAuthorized(context.Context, AuthorizedStatement) (int64, error) {
	panic("unexpected exec")
}
func (s *routineSession) ScanAuthorized(ctx context.Context, call AuthorizedStatement, dst ...any) error {
	s.calls++
	q, err := s.authority.Open(s, s.method, call)
	if err != nil {
		return err
	}
	return s.Scan(ctx, q.Query, q.Args, dst...)
}
func (s *routineSession) EachAuthorized(ctx context.Context, call AuthorizedStatement, decode func(func(...any) error) error) error {
	s.calls++
	q, err := s.authority.Open(s, s.method, call)
	if err != nil {
		return err
	}
	return s.Each(ctx, q.Query, q.Args, decode)
}

func TestTypedRoutineDeclaration(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	view := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	id, label := Input("id", Int8()), Input("label", Text())
	spec := RoutineSpec[int64]{Schema: "public", Name: `find"item`, Result: Int8(), Cardinality: RoutineOne, ReadOnly: true}
	params := []RoutineParameter{Parameter("name", Text()), Parameter("id", Int8()), Parameter("other", Int8())}
	r := DeclareRoutine(spec, params...)
	foreign := DeclareRoutine(spec, params...)
	params[0] = Parameter("changed", Bool())
	nameArg := Arg(FormalOf(r, "name", Text()), label.Expr())
	idArg := Arg(FormalOf(r, "id", Int8()), id.Expr())
	otherArg := Arg(FormalOf(r, "other", Int8()), id.Expr())
	p := DefineRoutine(view, "lookup", r, idArg, otherArg, nameArg)
	query := `SELECT (SELECT "public"."find""item"(CAST($1 AS "pg_catalog"."text"), CAST($2 AS "pg_catalog"."int8"), CAST($2 AS "pg_catalog"."int8"))) AS "id"`
	sig := `"public"."find""item"("pg_catalog"."text", "pg_catalog"."int8", "pg_catalog"."int8")`
	key := "lookup.call"
	snap := view.Definition()
	require.Equal(t, query, snap.Statements[key])
	require.Equal(t, "lookup", snap.StatementQueries[key])
	meta := snap.StatementShapes[key]
	require.Equal(t, []ShapeInput{{Name: "label", Type: Text().id.qualified()}, {Name: "id", Type: Int8().id.qualified()}}, meta.Inputs)
	require.Equal(t, []string{"pg_catalog", "public"}, meta.SchemaUsage)
	require.Equal(t, []DefinedPrivilege{{Function: sig, Privilege: "EXECUTE"}}, meta.Privileges)
	require.Equal(t, []RoutineRequirement{{Signature: sig, ResultType: Int8().id.qualified(), ReadOnly: true}}, meta.Routines)
	meta.Routines[0].ReadOnly = false
	require.True(t, view.Definition().StatementShapes[key].Routines[0].ReadOnly)
	require.True(t, p.query.shape.shape.Routines[0].ReadOnly)
	before := view.Definition()
	for _, bad := range [][]RoutineArgument{
		{idArg, nameArg}, {idArg, nameArg, otherArg, otherArg}, {idArg, nameArg, idArg}, {idArg, nameArg, {}},
		{idArg, nameArg, Arg(FormalOf(foreign, "other", Int8()), id.Expr())},
		{idArg, otherArg, Arg(FormalOf(r, "name", Text()), Input("label", CatalogType[string]("public", "kind", nil)).Expr())},
	} {
		_, err := TryDefineRoutine(view, "invalid", r, bad...)
		require.Error(t, err)
	}
	require.Equal(t, before, view.Definition())
	require.Panics(t, func() { FormalOf(r, "missing", Int8()) })
	require.Panics(t, func() { FormalOf(r, "name", Int8()) })
	require.Panics(t, func() { DeclareRoutine(spec, Parameter("a", Bool()), Parameter("a", Bool())) })
	for _, name := range []string{"pg_advisory_unlock_all", "pg_try_advisory_xact_lock_shared"} {
		require.Panics(t, func() {
			DeclareRoutine(RoutineSpec[int64]{Schema: "pg_catalog", Name: name, Result: Int8(), Cardinality: RoutineOne})
		})
	}
	nullable := spec
	nullable.Nullable = true
	_, err := TryDefineRoutine(view, "nullable", DeclareRoutine(nullable))
	require.ErrorContains(t, err, "incompatible result")
	var zero Routine[int64]
	_, err = TryDefineRoutine(view, "zero", &zero)
	require.Error(t, err)
}

func TestTypedRoutineDispatch(t *testing.T) {
	tracer := new(fakeTracer)
	reg := NewRegistry(RegistryConfig[customTenant]{Deny: DenyUnscopedWrites, Tracer: tracer})
	view := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	input := Input("id", Int8())
	for _, tc := range []struct {
		key    string
		card   RoutineCardinality
		read   bool
		method CallMethod
	}{{"one", RoutineOne, true, CallScan}, {"many", RoutineSet, true, CallEach}, {"write_one", RoutineOne, false, CallReturning}, {"write_many", RoutineSet, false, CallReturningEach}} {
		t.Run(tc.key, func(t *testing.T) {
			r := DeclareRoutine(RoutineSpec[int64]{Schema: "public", Name: "next_item", Result: Int8(), Cardinality: tc.card, ReadOnly: tc.read}, Parameter("id", Int8()))
			plan := DefineRoutine(view, tc.key, r, Arg(FormalOf(r, "id", Int8()), input.Expr()))
			s := &routineSession{authority: reg.SQLAuthority(), method: tc.method}
			s.scanFn = func(dst ...any) error {
				*dst[0].(*int64) = 4
				return nil
			}
			s.eachRows = [][]any{{int64(3)}, {int64(5)}}
			run := func(s Session[customTenant], args ...Binding) error {
				if tc.card == RoutineOne {
					row, err := plan.Bind(s).One(t.Context(), args...)
					if err == nil {
						require.EqualValues(t, 4, row.ID)
					}
					return err
				}
				rows, err := plan.Bind(s).All(t.Context(), args...)
				if err == nil {
					require.Equal(t, []typedResult{{3}, {5}}, rows)
				}
				return err
			}
			arg := BindValue(input, 7)
			require.NoError(t, run(s, arg))
			require.Equal(t, []any{int64(7)}, s.lastArgs)
			span := tracer.spans[len(tracer.spans)-1]
			require.Equal(t, "entries."+tc.key+".call", span.info.Key)
			require.NoError(t, span.err)
			require.Error(t, run(s, BindValue(Input("id", Int8()), 7)))
			if tc.card == RoutineOne {
				_, err := plan.Bind(s).All(t.Context(), arg)
				require.Error(t, err)
			} else {
				_, err := plan.Bind(s).One(t.Context(), arg)
				require.Error(t, err)
			}
			require.Equal(t, 1, s.calls)
			require.ErrorIs(t, run(new(fakeSession), arg), ErrStatementAuthority)
			if !tc.read {
				s.readOnly = true
				require.ErrorIs(t, run(s, arg), ErrReadOnly)
				s.readOnly = false
				s.unscoped = true
				require.ErrorIs(t, run(s, arg), ErrCapabilityDenied)
				require.Equal(t, 1, s.calls)
			}
		})
	}
}

func TestTypedRoutineScanError(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	view := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	fn := DeclareRoutine(RoutineSpec[int64]{Schema: "public", Name: "next_item", Result: Int8(), Cardinality: RoutineOne})
	plan := DefineRoutine(view, "next", fn)
	s := &routineSession{authority: reg.SQLAuthority(), method: CallReturning}
	bad := errors.New("decode")
	s.scanFn = func(dst ...any) error {
		*dst[0].(*int64) = 9
		return bad
	}
	row, err := plan.Bind(s).One(t.Context())
	require.ErrorIs(t, err, bad)
	require.Zero(t, row)
}

func TestTypedUnregisteredLockAndRoutine(t *testing.T) {
	s := new(fakeSession)
	var routine RoutinePlan[typedResult, customTenant]
	_, err := routine.Bind(s).One(t.Context())
	require.ErrorIs(t, err, ErrStatementAuthority)
	_, err = routine.Bind(s).All(t.Context())
	require.ErrorIs(t, err, ErrStatementAuthority)
	var rows RowsPlan[typedResult, customTenant]
	_, err = rows.Bind(s).Find(t.Context(), RowChoice{})
	require.ErrorIs(t, err, ErrStatementAuthority)
	var lock AdvisoryLockPlan[lockFlag, customTenant]
	_, err = lock.Bind(s).Acquire(t.Context())
	require.ErrorIs(t, err, ErrStatementAuthority)
	require.Empty(t, s.lastQuery)
}
