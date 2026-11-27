package tabula

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

type writeSessionState struct {
	operations, count, scans, visits int
	calls                            []AuthorizedSQL
	fail, scanFail, tailFail         error
	operationErr                     error
	rollback                         bool
}

type writeSession struct {
	fakeSession
	authority SQLAuthority
	state     *writeSessionState
	readOnly  bool
	child     bool
}

func (s *writeSession) ReadOnly() bool { return s.readOnly }
func (s *writeSession) WithinOperation(ctx context.Context, fn func(context.Context, Session[customTenant]) error) error {
	s.state.operations++
	previous := s.state.count
	child := &writeSession{authority: s.authority, state: s.state, child: true}
	err := fn(ctx, child)
	s.state.operationErr = err
	if err != nil {
		s.state.count, s.state.rollback = previous, true
	}
	return err
}
func (s *writeSession) ExecAuthorized(_ context.Context, call AuthorizedStatement) (int64, error) {
	query, err := s.authority.Open(s, CallExec, call)
	if err != nil {
		return 0, err
	}
	s.state.calls = append(s.state.calls, query)
	s.state.count++
	return 1, s.state.fail
}
func (s *writeSession) ScanAuthorized(context.Context, AuthorizedStatement, ...any) error {
	return errors.New("unexpected single-row write call")
}
func (s *writeSession) EachAuthorized(_ context.Context, call AuthorizedStatement, visit func(func(...any) error) error) error {
	if !s.child {
		return errors.New("returning call was not bound to child")
	}
	query, err := s.authority.Open(s, CallReturningEach, call)
	if err != nil {
		return err
	}
	s.state.calls = append(s.state.calls, query)
	s.state.count += 3
	for i := range 3 {
		s.state.visits++
		if err := visit(func(dest ...any) error {
			s.state.scans++
			*dest[0].(*int64) = int64(i + 1)
			return s.state.scanFail
		}); err != nil {
			return err
		}
	}
	return s.state.tailFail
}

func TestReturningOperationAndDrain(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	_, source := statementSource(reg, WriteDefault)
	id := FieldOf(source.Rel(), "id", Int8())
	input := Input("id", Int8())
	out := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	plan := DefineReturning(out, "inserted", WithReturning(InsertInto(source).Values(Assign(id, input.Expr())), Project(Output(out, "id", Int8()), id.Expr())))
	s := &writeSession{authority: reg.SQLAuthority(), state: new(writeSessionState)}
	rows, err := plan.Bind(s).All(t.Context(), BindValue(input, 9))
	require.NoError(t, err)
	require.Equal(t, []typedResult{{1}, {2}, {3}}, rows)
	require.Equal(t, 1, s.state.operations)
	require.Equal(t, 3, s.state.count)
	_, err = plan.Bind(s).All(t.Context())
	require.Error(t, err)
	require.Equal(t, 1, s.state.operations)
	s.state = new(writeSessionState)
	for row, err := range plan.Bind(s).Iter(t.Context(), BindValue(input, 9)) {
		require.NoError(t, err)
		require.EqualValues(t, 1, row.ID)
		break
	}
	require.Equal(t, 3, s.state.visits)
	require.Equal(t, 1, s.state.scans)
	require.Equal(t, 3, s.state.count)
	require.NoError(t, s.state.operationErr)
	s.state = &writeSessionState{scanFail: errors.New("decoder failure")}
	rows, err = plan.Bind(s).All(t.Context(), BindValue(input, 9))
	require.Error(t, err)
	require.Nil(t, rows)
	require.True(t, s.state.rollback)
	require.Zero(t, s.state.count)
	require.NotErrorIs(t, s.state.operationErr, ErrUnobservedWrite)
	s.state = &writeSessionState{tailFail: namedDiagnostic{pgStateErr: pgStateErr{code: "23514", msg: "late server failure"}, Constraint: "items_rank_check"}}
	for _, err := range plan.Bind(s).Iter(t.Context(), BindValue(input, 9)) {
		require.NoError(t, err)
		break
	}
	require.True(t, s.state.rollback)
	require.Zero(t, s.state.count)
	require.ErrorIs(t, s.state.operationErr, ErrUnobservedWrite)
	require.ErrorIs(t, s.state.operationErr, s.state.tailFail)
	require.Equal(t, "23514", SQLState(s.state.operationErr))
	require.Equal(t, "items_rank_check", ConstraintName(s.state.operationErr))
	s.state = &writeSessionState{tailFail: errors.New("visible server failure")}
	for _, err := range plan.Bind(s).Iter(t.Context(), BindValue(input, 9)) {
		if err != nil {
			require.ErrorIs(t, err, s.state.tailFail)
			require.NotErrorIs(t, err, ErrUnobservedWrite)
		}
	}
	require.True(t, s.state.rollback)
	require.NotErrorIs(t, s.state.operationErr, ErrUnobservedWrite)
	s.state = new(writeSessionState)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for _, err := range plan.Bind(s).Iter(ctx, BindValue(input, 9)) {
		require.NoError(t, err)
		cancel()
		break
	}
	require.ErrorIs(t, s.state.operationErr, ErrUnobservedWrite)
	require.ErrorIs(t, s.state.operationErr, context.Canceled)
	require.True(t, s.state.rollback)
	s.readOnly = true
	_, err = plan.Bind(s).All(t.Context(), BindValue(input, 9))
	require.ErrorIs(t, err, ErrReadOnly)
	missing := &routineSession{authority: reg.SQLAuthority(), method: CallReturningEach}
	_, err = plan.Bind(missing).All(t.Context(), BindValue(input, 9))
	require.ErrorIs(t, err, ErrOperationRequired)
	require.Zero(t, missing.calls)
}

func TestWriteBatchValidationAndCanonicalBindings(t *testing.T) {
	adapted := 0
	reg := NewRegistry(RegistryConfig[customTenant]{ArrayColumn: func(reflect.Type) (func(any) any, error) {
		return func(value any) any { return value }, nil
	}, ArrayArg: func(value any) any {
		adapted++
		v := value.([]int32)
		require.Equal(t, []int32{1, 2}, v)
		v[0] = 7
		return v
	}})
	type arrayItem struct {
		ID      int64   `db:"id,key"`
		Members []int32 `db:"members,array"`
	}
	table := DefinePlanSource(reg, TableSpec[arrayItem, int64]{Name: "public.items", RelationScope: Global()})
	src := MapTable(table, ColumnType("members", ArrayOf(Int4()))).As("i")
	id, array := FieldOf(src.Rel(), "id", Int8()), FieldOf(src.Rel(), "members", ArrayOf(Int4()))
	key, values := Input("id", Int8()), Input("values", ArrayOf(Int4()))
	plan := DefineWrite(src, "batch", InsertInto(src).Values(Assign(id, key.Expr()), Assign(array, values.Expr())).Batch(1, 2))
	raw := []int32{1, 2}
	canonical := BindValue(values, raw)
	raw[0] = 99
	rows := [][]Binding{{BindValue(key, 1), canonical}, {BindValue(key, 2), canonical}, {BindValue(key, 3), canonical}}
	s := &writeSession{authority: reg.SQLAuthority(), state: new(writeSessionState)}
	n, err := plan.Bind(s).ExecBatch(t.Context(), rows)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
	require.Equal(t, 3, adapted)
	require.Equal(t, 1, s.state.operations)
	require.Len(t, s.state.calls, 2)
	_, err = plan.Bind(s).ExecBatch(t.Context(), append(rows, []Binding{canonical}))
	require.ErrorContains(t, err, "missing bindings")
	require.Equal(t, 1, s.state.operations)
	require.Equal(t, 3, adapted)
	_, err = plan.Bind(s).ExecBatch(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, s.state.operations)
	s.state = &writeSessionState{fail: errors.New("write failed")}
	n, err = plan.Bind(s).ExecBatch(t.Context(), rows)
	require.Error(t, err)
	require.Zero(t, n)
	require.True(t, s.state.rollback)
	require.Zero(t, s.state.count)
	for _, sizes := range [][]int{{2}, {1, 1}, {1, 0}, {1, 65536}} {
		_, err := TryDefineWrite(src, "bad_batch", InsertInto(src).Values(Assign(id, key.Expr())).Batch(sizes[0], sizes[1:]...))
		require.Error(t, err)
	}
	_, err = TryDefineWrite(src, "too_many", InsertInto(src).Values(Assign(id, key.Expr()), Assign(array, values.Expr())).Batch(1, 32768))
	require.ErrorContains(t, err, "parameter limit")
}

func TestWriteBatchNestedLimitBeforeIO(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	_, src := statementSource(reg, WriteDefault)
	id := FieldOf(src.Rel(), "id", Int8())
	key, limit := Input("id", Int8()), Input("take", Int8())
	value := Coalesce(ScalarSubquery(SelectValue(key.Expr()).Limit(limit, 1)), key.Expr())
	plan := DefineWrite(src, "batch", InsertInto(src).Values(Assign(id, value)).Batch(1, 2))
	s := &writeSession{authority: reg.SQLAuthority(), state: new(writeSessionState)}
	row := func(keyValue, limitValue int64) []Binding {
		return []Binding{BindValue(key, keyValue), BindValue(limit, limitValue)}
	}
	_, err := plan.Bind(s).ExecBatch(t.Context(), [][]Binding{row(99, 1), row(100, 1)})
	require.NoError(t, err)
	require.Equal(t, 1, s.state.operations)
	_, err = plan.Bind(s).ExecBatch(t.Context(), [][]Binding{row(1, 1), row(2, 2)})
	require.ErrorContains(t, err, "limit")
	require.Equal(t, 1, s.state.operations)
}
