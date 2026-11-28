package tabula_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

type fakeAuthSession[S any] struct {
	scope      S
	unscoped   bool
	readOnly   bool
	proof      tabula.SessionProof
	authority  tabula.SQLAuthority
	execRows   int64
	execErr    error
	eachRows   [][]any
	eachErr    error
	scanVal    any
	scanErr    error
	lastSQL    tabula.AuthorizedSQL
	lastMethod tabula.CallMethod
	textCalls  int
	authCalls  int
	stoppedAt  int
}

func (s *fakeAuthSession[S]) Scope() S                          { return s.scope }
func (s *fakeAuthSession[S]) Unscoped() bool                    { return s.unscoped }
func (s *fakeAuthSession[S]) ReadOnly() bool                    { return s.readOnly }
func (s *fakeAuthSession[S]) SessionProof() tabula.SessionProof { return s.proof }
func (s *fakeAuthSession[S]) SQLAuthority() tabula.SQLAuthority { return s.authority }
func (s *fakeAuthSession[S]) Exec(context.Context, string, ...any) (int64, error) {
	s.textCalls++
	return 0, tabula.ErrLegacySQL
}
func (s *fakeAuthSession[S]) Scan(context.Context, string, []any, ...any) error {
	s.textCalls++
	return tabula.ErrLegacySQL
}
func (s *fakeAuthSession[S]) Returning(context.Context, string, []any, ...any) error {
	s.textCalls++
	return tabula.ErrLegacySQL
}
func (s *fakeAuthSession[S]) Each(context.Context, string, []any, func(func(...any) error) error) error {
	s.textCalls++
	return tabula.ErrLegacySQL
}

func (s *fakeAuthSession[S]) ExecAuthorized(_ context.Context, call tabula.AuthorizedStatement) (int64, error) {
	q, err := s.authority.Open(s, tabula.CallExec, call)
	if err != nil {
		return 0, err
	}
	s.authCalls++
	s.lastSQL = q
	s.lastMethod = tabula.CallExec
	return s.execRows, s.execErr
}

func (s *fakeAuthSession[S]) ScanAuthorized(_ context.Context, call tabula.AuthorizedStatement, dest ...any) error {
	method := tabula.CallScan
	if call.Method() == tabula.CallReturning {
		method = tabula.CallReturning
	}
	q, err := s.authority.Open(s, method, call)
	if err != nil {
		return err
	}
	s.authCalls++
	s.lastSQL = q
	s.lastMethod = method
	if s.scanErr != nil {
		return s.scanErr
	}
	if len(dest) == 1 {
		if p, ok := dest[0].(*int64); ok {
			if v, ok := s.scanVal.(int64); ok {
				*p = v
			}
		}
	}
	return nil
}

func (s *fakeAuthSession[S]) EachAuthorized(_ context.Context, call tabula.AuthorizedStatement, visit func(func(...any) error) error) error {
	method := tabula.CallEach
	if call.Method() == tabula.CallReturningEach {
		method = tabula.CallReturningEach
	}
	q, err := s.authority.Open(s, method, call)
	if err != nil {
		return err
	}
	s.authCalls++
	s.lastSQL = q
	s.lastMethod = method
	if s.eachErr != nil {
		return s.eachErr
	}
	for idx, row := range s.eachRows {
		if err := visit(func(dest ...any) error {
			for i, d := range dest {
				switch p := d.(type) {
				case *int64:
					*p = row[i].(int64)
				case *int:
					*p = row[i].(int)
				case *string:
					*p = row[i].(string)
				case *bool:
					*p = row[i].(bool)
				case *time.Time:
					*p = row[i].(time.Time)
				case **time.Time:
					if row[i] == nil {
						*p = nil
					} else {
						v := row[i].(time.Time)
						*p = &v
					}
				case **string:
					if row[i] == nil {
						*p = nil
					} else {
						v := row[i].(string)
						*p = &v
					}
				case *[]string:
					*p = row[i].([]string)
				case sql.Scanner:
					if err := p.Scan(row[i]); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			s.stoppedAt = idx + 1
			return err
		}
	}
	return nil
}

type customUUID [16]byte

func (u customUUID) Value() (driver.Value, error) { return "00000000-0000-0000-0000-000000000001", nil }
func (u *customUUID) Scan(src any) error          { return nil }

type unmappedCustom struct{ V string }

func (u unmappedCustom) Value() (driver.Value, error) { return u.V, nil }
func (u *unmappedCustom) Scan(src any) error          { return nil }

func TestDefineSQLValidationAndExecution(t *testing.T) {
	type itemRow struct {
		ID   int64  `db:"id"`
		Name string `db:"name"`
	}
	type itemArgs struct {
		ID     int64  `db:"id"`
		Secret string `db:"secret,secret"`
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	key := tabula.NewSessionKey()
	reg := tabula.NewRegistry(tabula.RegistryConfig[int64]{
		Deny:       tabula.DenyLegacySQL,
		SessionKey: key,
		Log: tabula.LogConfig{
			Logger: logger,
			Level:  tabula.LogAll,
			Values: tabula.ValuesAllowed,
			SQL:    true,
		},
		SessionGuard: tabula.SessionGuardConfig{
			VariablePrefixes:    []string{"demo."},
			ForbidVariableReads: true,
		},
	})

	q := tabula.DefineSQL[itemRow, itemArgs](reg, tabula.SQLSpec{
		Name:  "items.get",
		SQL:   "SELECT id, name FROM public.items WHERE tenant_id = @tenant_id AND id = @id AND token = @secret",
		Scope: tabula.ScopeParam("tenant_id"),
	})
	require.Equal(t, "items.get", q.Name())
	require.Equal(t, "SELECT id, name FROM public.items WHERE tenant_id = $1 AND id = $2 AND token = $3", q.SQL())

	def := q.Definition()
	require.Len(t, def.Statements, 1)
	shape := def.StatementShapes["sql"]
	require.Equal(t, tabula.EffectRead, shape.Effects)
	require.Len(t, shape.Inputs, 3)
	require.True(t, shape.Inputs[0].Scope)
	require.Equal(t, []string{"smallint", "integer", "bigint"}, shape.Inputs[0].AllowedTypes)
	require.False(t, shape.Inputs[1].Secret)
	require.True(t, shape.Inputs[2].Secret)
	require.Len(t, shape.Result, 2)
	require.Equal(t, "id", shape.Result[0].Name)
	require.Equal(t, "name", shape.Result[1].Name)

	// Verify snapshot round-trip preserves AllowedTypes.
	snap := sqlcheck.RenderAllSnapshot(reg.Defined(), reg.DefinedViews())
	parsed, err := sqlcheck.ParseSnapshot(snap)
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	require.Equal(t, shape.Inputs, parsed[0].StatementShapes["sql"].Inputs)
	require.Equal(t, shape.Result, parsed[0].StatementShapes["sql"].Result)

	// Authorized execution with One / MaybeOne / First / All / Each and redaction.
	sess := &fakeAuthSession[int64]{
		scope:     42,
		authority: reg.SQLAuthority(),
		eachRows:  [][]any{{int64(7), "alpha"}},
	}
	sess.proof = key.Sign(sess)
	bound := q.Bind(sess)

	row, err := bound.One(t.Context(), itemArgs{ID: 7, Secret: "top-secret-token"})
	require.NoError(t, err)
	require.Equal(t, itemRow{ID: 7, Name: "alpha"}, row)
	require.Zero(t, sess.textCalls)
	require.Equal(t, []any{int64(42), int64(7), "top-secret-token"}, sess.lastSQL.Args)
	require.Equal(t, []bool{false, false, true}, sess.lastSQL.SecretArgs)
	require.NotContains(t, logBuf.String(), "top-secret-token")
	require.Contains(t, logBuf.String(), "[redacted]")

	// ScopeParam fails closed on unscoped session.
	unscopedSess := &fakeAuthSession[int64]{unscoped: true, authority: reg.SQLAuthority()}
	unscopedSess.proof = key.Sign(unscopedSess)
	_, err = q.Bind(unscopedSess).One(t.Context(), itemArgs{ID: 7, Secret: "x"})
	require.ErrorIs(t, err, tabula.ErrStatementAuthority)

	// Foreign session rejected by SessionKey.
	foreignSess := &fakeAuthSession[int64]{scope: 42, authority: reg.SQLAuthority()}
	_, err = q.Bind(foreignSess).One(t.Context(), itemArgs{ID: 7, Secret: "x"})
	require.ErrorIs(t, err, tabula.ErrForeignSession)

	// 0 rows behavior: One -> ErrNotFound (matches sql.ErrNoRows), MaybeOne -> false, First -> false.
	sess.eachRows = nil
	_, err = bound.One(t.Context(), itemArgs{ID: 7, Secret: "x"})
	require.ErrorIs(t, err, tabula.ErrNotFound)
	require.ErrorIs(t, err, sql.ErrNoRows)
	_, ok, err := bound.MaybeOne(t.Context(), itemArgs{ID: 7, Secret: "x"})
	require.NoError(t, err)
	require.False(t, ok)
	_, ok, err = bound.First(t.Context(), itemArgs{ID: 7, Secret: "x"})
	require.NoError(t, err)
	require.False(t, ok)

	// >1 rows behavior: One/MaybeOne -> ErrMultipleRows; First -> first row & stops cursor; All -> all; Each -> callback error preserved.
	sess.eachRows = [][]any{{int64(1), "a"}, {int64(2), "b"}}
	_, err = bound.One(t.Context(), itemArgs{ID: 7, Secret: "x"})
	require.ErrorIs(t, err, tabula.ErrMultipleRows)
	_, ok, err = bound.MaybeOne(t.Context(), itemArgs{ID: 7, Secret: "x"})
	require.ErrorIs(t, err, tabula.ErrMultipleRows)
	require.False(t, ok)

	first, ok, err := bound.First(t.Context(), itemArgs{ID: 7, Secret: "x"})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, itemRow{ID: 1, Name: "a"}, first)
	require.Equal(t, 1, sess.stoppedAt)

	all, err := bound.All(t.Context(), itemArgs{ID: 7, Secret: "x"})
	require.NoError(t, err)
	require.Equal(t, []itemRow{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}}, all)

	sentinel := errors.New("app callback stop")
	err = bound.Each(t.Context(), itemArgs{ID: 7, Secret: "x"}, func(r itemRow) error {
		return sentinel
	})
	require.Same(t, sentinel, err)
}

func TestDefineSQLRowLockReadOnlyAndRejections(t *testing.T) {
	type row struct {
		ID int64 `db:"id"`
	}
	type idArgs struct {
		ID int64 `db:"id"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[int64]{
		Deny: tabula.DenyLegacySQL,
		SessionGuard: tabula.SessionGuardConfig{
			VariablePrefixes:    []string{"demo."},
			ForbidVariableReads: true,
		},
	})

	for i, clause := range []string{"FOR UPDATE", "FOR NO KEY UPDATE", "FOR SHARE", "FOR KEY SHARE"} {
		q := tabula.DefineSQL[row, idArgs](reg, tabula.SQLSpec{
			Name:  "lock_" + clause,
			SQL:   "SELECT id FROM public.items WHERE id = @id " + clause,
			Scope: tabula.ScopeByRLS(),
		})
		require.NotZero(t, q.Definition().StatementShapes["sql"].Effects&tabula.EffectRowLock, i)
		ro := &fakeAuthSession[int64]{scope: 1, readOnly: true, authority: reg.SQLAuthority()}
		_, err := q.Bind(ro).One(t.Context(), idArgs{ID: 1})
		require.ErrorIs(t, err, tabula.ErrReadOnly)
	}

	// Rejections at TryDefineSQL time.
	badSpecs := []struct {
		name string
		spec tabula.SQLSpec
		want string
	}{
		{
			name: "missing scope on scoped registry",
			spec: tabula.SQLSpec{Name: "bad_scope", SQL: "SELECT id FROM public.items WHERE id = @id"},
			want: "explicit Scope",
		},
		{
			name: "raw positional placeholder",
			spec: tabula.SQLSpec{Name: "bad_pos", SQL: "SELECT id FROM public.items WHERE id = $1", Scope: tabula.ScopeGlobal()},
			want: "positional parameter $1",
		},
		{
			name: "unknown named parameter",
			spec: tabula.SQLSpec{Name: "bad_param", SQL: "SELECT id FROM public.items WHERE id = @id AND code = @missing", Scope: tabula.ScopeGlobal()},
			want: "unknown parameter @missing",
		},
		{
			name: "unused args field",
			spec: tabula.SQLSpec{Name: "bad_unused", SQL: "SELECT id FROM public.items", Scope: tabula.ScopeGlobal()},
			want: "not referenced in SQL",
		},
		{
			name: "multi-statement",
			spec: tabula.SQLSpec{Name: "bad_multi", SQL: "SELECT id FROM public.items WHERE id = @id; DELETE FROM public.items", Scope: tabula.ScopeGlobal()},
			want: "multiple SQL statements",
		},
		{
			name: "set_config blocked",
			spec: tabula.SQLSpec{Name: "bad_set_config", SQL: "SELECT set_config('demo.x', '1', true) AS id WHERE 1 = @id", Scope: tabula.ScopeGlobal()},
			want: "set_config",
		},
		{
			name: "protected variable read blocked by Registry SessionGuard",
			spec: tabula.SQLSpec{Name: "bad_var_read", SQL: "SELECT current_setting('demo.scope')::bigint AS id WHERE 1 = @id", Scope: tabula.ScopeGlobal()},
			want: "current_setting",
		},
		{
			name: "write statement rejected in DefineSQL",
			spec: tabula.SQLSpec{Name: "bad_write", SQL: "DELETE FROM public.items WHERE id = @id RETURNING id", Scope: tabula.ScopeGlobal()},
			want: "read-only queries only",
		},
		{
			name: "modifying CTE rejected in DefineSQL",
			spec: tabula.SQLSpec{Name: "bad_mcte", SQL: "WITH d AS (DELETE FROM public.items WHERE id = @id RETURNING id) SELECT id FROM d", Scope: tabula.ScopeGlobal()},
			want: "read-only queries only",
		},
		{
			name: "ScopeParam conflicts with Args field",
			spec: tabula.SQLSpec{Name: "bad_scope_dup", SQL: "SELECT id FROM public.items WHERE id = @id", Scope: tabula.ScopeParam("id")},
			want: "must not also be declared on Args",
		},
		{
			name: "ScopeParam not referenced in SQL",
			spec: tabula.SQLSpec{Name: "bad_scope_unused", SQL: "SELECT id FROM public.items WHERE id = @id", Scope: tabula.ScopeParam("tenant_id")},
			want: "ScopeParam @tenant_id is not referenced",
		},
		{
			name: "duplicate name",
			spec: tabula.SQLSpec{Name: "lock_FOR UPDATE", SQL: "SELECT id FROM public.items WHERE id = @id", Scope: tabula.ScopeGlobal()},
			want: "already registered",
		},
		{
			name: "session-level advisory lock rejected",
			spec: tabula.SQLSpec{Name: "bad_sess_lock", SQL: "SELECT pg_advisory_lock(@id) AS id", Scope: tabula.ScopeGlobal()},
			want: "session-level advisory lock",
		},
	}
	for _, tc := range badSpecs {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tabula.TryDefineSQL[row, idArgs](reg, tc.spec)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}

	// Untagged exported field and duplicate db tags rejected.
	type untaggedArgs struct {
		ID int64
	}
	_, err := tabula.TryDefineSQL[row, untaggedArgs](reg, tabula.SQLSpec{
		Name: "untagged", SQL: "SELECT id FROM public.items", Scope: tabula.ScopeGlobal(),
	})
	require.ErrorContains(t, err, "has no db tag")

	type dupTagArgs struct {
		A int64 `db:"id"`
		B int64 `db:"id"`
	}
	_, err = tabula.TryDefineSQL[row, dupTagArgs](reg, tabula.SQLSpec{
		Name: "dup_tag", SQL: "SELECT id FROM public.items WHERE id = @id", Scope: tabula.ScopeGlobal(),
	})
	require.ErrorContains(t, err, `declared twice`)
}

func TestDefineSQLExecAndReturning(t *testing.T) {
	type row struct {
		ID int64 `db:"id"`
	}
	type args struct {
		ID int64 `db:"id"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{
		Deny: tabula.DenyLegacySQL | tabula.DenyUnscopedWrites,
	})

	// Exec accepts INSERT/UPDATE/DELETE/MERGE/modifying CTE without RETURNING and AdvisoryLock.
	del := tabula.DefineSQLExec[args](reg, tabula.SQLSpec{
		Name: "items.delete",
		SQL:  "DELETE FROM public.items WHERE id = @id",
	})
	require.Equal(t, tabula.EffectWrite, del.Definition().StatementShapes["sql"].Effects)

	adv := tabula.DefineSQLExec[args](reg, tabula.SQLSpec{
		Name:         "items.lock_xact",
		SQL:          "SELECT pg_advisory_xact_lock(@id)",
		AdvisoryLock: true,
	})
	require.Equal(t, tabula.EffectRead|tabula.EffectAdvisoryLock, adv.Definition().StatementShapes["sql"].Effects)

	// Exec rejects RETURNING, plain SELECT without AdvisoryLock, and session advisory lock.
	_, err := tabula.TryDefineSQLExec[args](reg, tabula.SQLSpec{
		Name: "bad_ret", SQL: "DELETE FROM public.items WHERE id = @id RETURNING id",
	})
	require.ErrorContains(t, err, "DefineSQLReturning")

	_, err = tabula.TryDefineSQLExec[args](reg, tabula.SQLSpec{
		Name: "bad_sel", SQL: "SELECT id FROM public.items WHERE id = @id",
	})
	require.ErrorContains(t, err, "AdvisoryLock: true")

	_, err = tabula.TryDefineSQLExec[args](reg, tabula.SQLSpec{
		Name: "bad_adv", SQL: "SELECT pg_advisory_lock(@id)", AdvisoryLock: true,
	})
	require.ErrorContains(t, err, "session-level advisory lock")

	// Returning accepts RETURNING and modifying CTE returning rows; rejects plain SELECT and non-RETURNING writes.
	ret := tabula.DefineSQLReturning[row, args](reg, tabula.SQLSpec{
		Name: "items.delete_returning",
		SQL:  "DELETE FROM public.items WHERE id = @id RETURNING id",
	})
	mcte := tabula.DefineSQLReturning[row, args](reg, tabula.SQLSpec{
		Name: "items.move_returning",
		SQL:  "WITH moved AS (DELETE FROM public.items WHERE id = @id RETURNING id) SELECT id FROM moved",
	})
	require.Equal(t, tabula.EffectWrite, ret.Definition().StatementShapes["sql"].Effects)
	require.Equal(t, tabula.EffectWrite, mcte.Definition().StatementShapes["sql"].Effects)

	_, err = tabula.TryDefineSQLReturning[row, args](reg, tabula.SQLSpec{
		Name: "bad_plain_sel", SQL: "SELECT id FROM public.items WHERE id = @id",
	})
	require.ErrorContains(t, err, "DefineSQL")

	_, err = tabula.TryDefineSQLReturning[row, args](reg, tabula.SQLSpec{
		Name: "bad_no_ret", SQL: "DELETE FROM public.items WHERE id = @id",
	})
	require.ErrorContains(t, err, "DefineSQLExec")

	// Exec & Returning respect ReadOnly and DenyUnscopedWrites.
	sess := &fakeAuthSession[struct{}]{authority: reg.SQLAuthority(), execRows: 3, eachRows: [][]any{{int64(9)}}}
	n, err := del.Bind(sess).Exec(t.Context(), args{ID: 9})
	require.NoError(t, err)
	require.Equal(t, int64(3), n)

	got, err := mcte.Bind(sess).One(t.Context(), args{ID: 9})
	require.NoError(t, err)
	require.Equal(t, row{ID: 9}, got)
	require.Equal(t, tabula.CallReturningEach, sess.lastMethod)

	// StatementGuard with AllowAuthorizedWriteCTE permits modifying CTE via CheckAuthorized.
	guard, err := (tabula.StatementPolicy{
		Read:                    []string{"select"},
		Write:                   []string{"delete", "with"},
		RequireReturning:        true,
		AllowAuthorizedWriteCTE: true,
	}).Compile(true)
	require.NoError(t, err)
	require.NoError(t, guard.Check("Scan", "SELECT 1"))

	// Unscoped write denied when DenyUnscopedWrites is set.
	unscoped := &fakeAuthSession[struct{}]{unscoped: true, authority: reg.SQLAuthority()}
	_, err = del.Bind(unscoped).Exec(t.Context(), args{ID: 9})
	require.ErrorIs(t, err, tabula.ErrCapabilityDenied)

	// ReadOnly rejects write.
	ro := &fakeAuthSession[struct{}]{readOnly: true, authority: reg.SQLAuthority()}
	_, err = ret.Bind(ro).One(t.Context(), args{ID: 9})
	require.ErrorIs(t, err, tabula.ErrReadOnly)
}

func TestTypeFamiliesDerivationAndCustomRegistration(t *testing.T) {
	type codeAlias string
	type wideRow struct {
		ID      int64           `db:"id"`
		Small   int16           `db:"small"`
		Mid     int32           `db:"mid"`
		Flag    bool            `db:"flag"`
		Ratio   float64         `db:"ratio"`
		Code    codeAlias       `db:"code"`
		At      time.Time       `db:"at"`
		Raw     []byte          `db:"raw"`
		Meta    json.RawMessage `db:"meta"`
		Payload map[string]any  `db:"payload,json"`
		Tags    []string        `db:"tags,array"`
		Addr    netip.Addr      `db:"addr"`
		CIDR    netip.Prefix    `db:"cidr"`
		UID     customUUID      `db:"uid"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{
		ArrayColumn: func(ft reflect.Type) (func(any) any, error) {
			return func(p any) any { return p }, nil
		},
		TypeFamilies: []tabula.TypeFamily{
			tabula.TypeFamilyOf[customUUID]("uuid"),
		},
	})
	tabula.RegisterTypeFamily[codeAlias](reg, "character varying")

	q := tabula.DefineSQL[wideRow, tabula.NoArgs](reg, tabula.SQLSpec{
		Name: "wide.select",
		SQL:  "SELECT id, small, mid, flag, ratio, code, at, raw, meta, payload, tags, addr, cidr, uid FROM public.wide",
	})
	res := q.Definition().StatementShapes["sql"].Result
	byName := map[string][]string{}
	for _, f := range res {
		byName[f.Name] = f.AllowedTypes
	}
	require.Equal(t, []string{"smallint", "integer", "bigint"}, byName["id"])
	require.Equal(t, []string{"smallint"}, byName["small"])
	require.Equal(t, []string{"smallint", "integer"}, byName["mid"])
	require.Equal(t, []string{"boolean"}, byName["flag"])
	require.Equal(t, []string{"real", "double precision"}, byName["ratio"])
	require.Equal(t, []string{"character varying"}, byName["code"])
	require.Equal(t, []string{"timestamp with time zone", "timestamp without time zone", "date"}, byName["at"])
	require.Equal(t, []string{"bytea"}, byName["raw"])
	require.Equal(t, []string{"json", "jsonb"}, byName["meta"])
	require.Equal(t, []string{"json", "jsonb"}, byName["payload"])
	require.Equal(t, []string{"text[]", "character varying[]", "character[]", "name[]", "citext[]"}, byName["tags"])
	require.Equal(t, []string{"inet"}, byName["addr"])
	require.Equal(t, []string{"cidr", "inet"}, byName["cidr"])
	require.Equal(t, []string{"uuid"}, byName["uid"])

	// Unmapped custom struct fails closed.
	type badRow struct {
		X unmappedCustom `db:"x"`
	}
	_, err := tabula.TryDefineSQL[badRow, tabula.NoArgs](reg, tabula.SQLSpec{
		Name: "bad.unmapped",
		SQL:  "SELECT x FROM public.wide",
	})
	require.ErrorContains(t, err, "no registered PostgreSQL type family")
}

func TestDefineSQLIntegerDirectionFamilies(t *testing.T) {
	type narrowStatus int16
	type mappedRank int32
	type intDirRow struct {
		I8       int8            `db:"i8"`
		I16      int16           `db:"i16"`
		I16Ptr   *int16          `db:"i16_ptr"`
		NullI16  sql.NullInt16   `db:"null_i16"`
		NullByte sql.NullByte    `db:"null_byte"`
		GenI16   sql.Null[int16] `db:"gen_i16"`
		I32      int32           `db:"i32"`
		I32Ptr   *int32          `db:"i32_ptr"`
		NullI32  sql.NullInt32   `db:"null_i32"`
		GenI32   sql.Null[int32] `db:"gen_i32"`
		U8       uint8           `db:"u8"`
		U16      uint16          `db:"u16"`
		U32      uint32          `db:"u32"`
		I64      int64           `db:"i64"`
		NullI64  sql.NullInt64   `db:"null_i64"`
		U64      uint64          `db:"u64"`
		ArrI16   []int16         `db:"arr_i16,array"`
		ArrI32   []int32         `db:"arr_i32,array"`
		Custom   narrowStatus    `db:"custom"`
		Mapped   mappedRank      `db:"mapped"`
	}
	type intDirArgs struct {
		I8       int8            `db:"i8"`
		I16      int16           `db:"i16"`
		I16Ptr   *int16          `db:"i16_ptr"`
		NullI16  sql.NullInt16   `db:"null_i16"`
		NullByte sql.NullByte    `db:"null_byte"`
		GenI16   sql.Null[int16] `db:"gen_i16"`
		I32      int32           `db:"i32"`
		I32Ptr   *int32          `db:"i32_ptr"`
		NullI32  sql.NullInt32   `db:"null_i32"`
		GenI32   sql.Null[int32] `db:"gen_i32"`
		U8       uint8           `db:"u8"`
		U16      uint16          `db:"u16"`
		U32      uint32          `db:"u32"`
		I64      int64           `db:"i64"`
		NullI64  sql.NullInt64   `db:"null_i64"`
		U64      uint64          `db:"u64"`
		ArrI16   []int16         `db:"arr_i16,array"`
		ArrI16P  []*int16        `db:"arr_i16_ptr,array"`
		ArrI32   []int32         `db:"arr_i32,array"`
		Custom   narrowStatus    `db:"custom"`
		Mapped   mappedRank      `db:"mapped"`
	}

	reg := tabula.NewRegistry(tabula.RegistryConfig[int16]{
		ArrayColumn: func(ft reflect.Type) (func(any) any, error) {
			return func(p any) any { return p }, nil
		},
		TypeFamilies: []tabula.TypeFamily{
			tabula.TypeFamilyOf[narrowStatus]("smallint"),
		},
		DefaultTypes: []tabula.TypeDefault{
			tabula.TypeMapping(tabula.CatalogType[mappedRank]("pg_catalog", "int4", nil)),
		},
	})

	q := tabula.DefineSQL[intDirRow, intDirArgs](reg, tabula.SQLSpec{
		Name: "ints.directions",
		SQL: `SELECT i8, i16, i16_ptr, null_i16, null_byte, gen_i16, i32, i32_ptr, null_i32, gen_i32,
 u8, u16, u32, i64, null_i64, u64, arr_i16, arr_i32, custom, mapped
FROM public.items
WHERE scope_id = @scope_id
  AND i8 = @i8 AND i16 = @i16 AND i16_ptr = @i16_ptr AND null_i16 = @null_i16 AND null_byte = @null_byte AND gen_i16 = @gen_i16
  AND i32 = @i32 AND i32_ptr = @i32_ptr AND null_i32 = @null_i32 AND gen_i32 = @gen_i32
  AND u8 = @u8 AND u16 = @u16 AND u32 = @u32 AND i64 = @i64 AND null_i64 = @null_i64 AND u64 = @u64
  AND arr_i16 = @arr_i16 AND arr_i16 = @arr_i16_ptr AND arr_i32 = @arr_i32
  AND custom = @custom AND mapped = @mapped`,
		Scope: tabula.ScopeParam("scope_id"),
	})

	wideInt := []string{"smallint", "integer", "bigint"}
	wideIntArr := []string{"smallint[]", "integer[]", "bigint[]"}
	oidInt := []string{"oid", "smallint", "integer", "bigint"}

	// Verify both live Definition and snapshot round-trip preserve asymmetric Input vs Result families.
	snap := sqlcheck.RenderAllSnapshot(reg.Defined(), reg.DefinedViews())
	parsed, err := sqlcheck.ParseSnapshot(snap)
	require.NoError(t, err)
	require.Len(t, parsed, 1)

	for _, shape := range []tabula.StatementShape{
		q.Definition().StatementShapes["sql"],
		parsed[0].StatementShapes["sql"],
	} {
		resByName := map[string][]string{}
		for _, f := range shape.Result {
			resByName[f.Name] = f.AllowedTypes
		}
		// Result direction keeps strict narrowing bounds.
		for _, k := range []string{"i8", "i16", "i16_ptr", "null_i16", "null_byte", "gen_i16"} {
			require.Equal(t, []string{"smallint"}, resByName[k], "result %s", k)
		}
		for _, k := range []string{"i32", "i32_ptr", "null_i32", "gen_i32", "u8", "u16"} {
			require.Equal(t, []string{"smallint", "integer"}, resByName[k], "result %s", k)
		}
		for _, k := range []string{"i64", "null_i64", "u64"} {
			require.Equal(t, wideInt, resByName[k], "result %s", k)
		}
		require.Equal(t, oidInt, resByName["u32"])
		require.Equal(t, []string{"smallint[]"}, resByName["arr_i16"])
		require.Equal(t, []string{"smallint[]", "integer[]"}, resByName["arr_i32"])
		require.Equal(t, []string{"smallint"}, resByName["custom"])
		require.Equal(t, []string{`"pg_catalog"."int4"`}, resByName["mapped"])

		inByName := map[string][]string{}
		for _, in := range shape.Inputs {
			inByName[in.Name] = in.AllowedTypes
		}
		// Input direction allows lossless widening, including ScopeParam, Null[T] and array elements.
		for _, k := range []string{
			"scope_id", "i8", "i16", "i16_ptr", "null_i16", "null_byte", "gen_i16",
			"i32", "i32_ptr", "null_i32", "gen_i32", "u8", "u16", "i64", "null_i64", "u64",
		} {
			require.Equal(t, wideInt, inByName[k], "input %s", k)
		}
		require.Equal(t, oidInt, inByName["u32"])
		for _, k := range []string{"arr_i16", "arr_i16_ptr", "arr_i32"} {
			require.Equal(t, wideIntArr, inByName[k], "input %s", k)
		}
		// Custom TypeFamilies and DefaultTypes remain unchanged on input.
		require.Equal(t, []string{"smallint"}, inByName["custom"])
		require.Equal(t, []string{`"pg_catalog"."int4"`}, inByName["mapped"])
	}

	// ScopeID fallback deriving from a struct scope returning int16 also uses Input direction.
	type wrappedScope struct{ id int16 }
	regWrapped := tabula.NewRegistry(tabula.RegistryConfig[wrappedScope]{
		ScopeID: func(s wrappedScope) any { return s.id },
	})
	type idRow struct {
		ID int64 `db:"id"`
	}
	qw := tabula.DefineSQL[idRow, tabula.NoArgs](regWrapped, tabula.SQLSpec{
		Name:  "ints.wrapped_scope",
		SQL:   "SELECT id FROM public.items WHERE group_id = @group_id",
		Scope: tabula.ScopeParam("group_id"),
	})
	require.Equal(t, wideInt, qw.Definition().StatementShapes["sql"].Inputs[0].AllowedTypes)
}

type unitWrappedScope struct {
	raw string
}

type unitWrappedScopeCodec struct {
	p *unitWrappedScope
}

func (c *unitWrappedScopeCodec) Scan(src any) error {
	switch v := src.(type) {
	case string:
		c.p.raw = v
		return nil
	case []byte:
		c.p.raw = string(v)
		return nil
	default:
		return errors.New("unexpected wrapped scope scan type")
	}
}

func (c *unitWrappedScopeCodec) Value() (driver.Value, error) {
	return c.p.raw, nil
}

func TestDefineSQLSharedTableRowTags(t *testing.T) {
	type auditCols struct {
		CreatedAt time.Time `db:"created_at,once"`
	}
	type sharedItem struct {
		ID        int64            `db:"id,key,serial"`
		ScopeID   unitWrappedScope `db:"scope_id,scope"`
		Code      string           `db:"code,once"`
		Name      string           `db:"name"`
		Version   int              `db:"version,version"`
		DeletedAt *time.Time       `db:"deleted_at,deleted"`
		Note      string           `db:"note,once,zero"`
		Tags      []string         `db:"tags,once,array"`
		Meta      map[string]any   `db:"meta,once,json"`
		Token     string           `db:"token,once,secret"`
		auditCols
	}
	type embeddedListRow struct {
		sharedItem
		Extra *string `db:"extra"`
	}
	type joinPairRow struct {
		Left  sharedItem `db:"l_"`
		Right sharedItem `db:"r_"`
	}
	type codeArgs struct {
		Code string `db:"code"`
	}

	key := tabula.NewSessionKey()
	reg := tabula.NewRegistry(tabula.RegistryConfig[unitWrappedScope]{
		SessionKey: key,
		Deny:       tabula.DenyLegacySQL,
		ScopeID:    func(s unitWrappedScope) any { return s.raw },
		ScopeWrap:  func(p *unitWrappedScope) any { return &unitWrappedScopeCodec{p: p} },
		ArrayColumn: func(ft reflect.Type) (func(any) any, error) {
			return func(p any) any { return p }, nil
		},
		TypeFamilies: []tabula.TypeFamily{
			tabula.TypeFamilyOf[unitWrappedScope]("uuid"),
		},
	})

	// Same struct works on Define, DefineSQL, DefineSQLReturning, DefineSQLList,
	// embedded row structs, and prefixed nested JOIN structs.
	tbl := tabula.Define(reg, tabula.TableSpec[sharedItem, int64]{
		Name: "public.items",
	})
	require.Equal(t, "public.items", tbl.Name())

	const cols = "id, scope_id, code, name, version, deleted_at, note, tags, meta, token, created_at"
	qRead := tabula.DefineSQL[sharedItem, codeArgs](reg, tabula.SQLSpec{
		Name:  "items.shared_read",
		SQL:   "SELECT " + cols + " FROM public.items WHERE scope_id = @scope_id AND code = @code",
		Scope: tabula.ScopeParam("scope_id"),
	})
	qRet := tabula.DefineSQLReturning[sharedItem, codeArgs](reg, tabula.SQLSpec{
		Name:  "items.shared_ret",
		SQL:   "UPDATE public.items SET name = 'x' WHERE scope_id = @scope_id AND code = @code RETURNING " + cols,
		Scope: tabula.ScopeParam("scope_id"),
	})
	qList := tabula.DefineSQLList[embeddedListRow, tabula.NoArgs](reg, tabula.SQLListSpec{
		Name:      "items.shared_list",
		Select:    "SELECT " + cols + ", 'extra'::text AS extra FROM public.items",
		Where:     "scope_id = @scope_id",
		CountBase: "SELECT count(*) FROM public.items",
		Scope:     tabula.ScopeParam("scope_id"),
		Order: tabula.QueryOrderSpec{
			Default:          "id",
			DefaultDirection: tabula.SortAsc,
			Sorts: []tabula.QuerySort{
				{Key: "id", Expr: "id", Column: "id"},
				{Key: "scope", Expr: "scope_id", Column: "scope_id"},
			},
			TieBreaker: []tabula.QuerySortTerm{{Expr: "id", Column: "id"}},
		},
		MaxLimit: 50,
		Keyset:   true,
	})
	qJoin := tabula.DefineSQL[joinPairRow, tabula.NoArgs](reg, tabula.SQLSpec{
		Name:  "items.shared_join",
		SQL:   "SELECT l.id AS l_id, r.id AS r_id FROM public.items l JOIN public.items r ON r.id = l.id WHERE l.scope_id = @scope_id",
		Scope: tabula.ScopeParam("scope_id"),
	})

	// Verify StatementShape types, ScopeWrap type family, and secret preservation.
	readShape := qRead.Definition().StatementShapes["sql"]
	require.Equal(t, []string{"uuid"}, readShape.Inputs[0].AllowedTypes)
	require.Len(t, readShape.Result, 11)
	require.Equal(t, "scope_id", readShape.Result[1].Name)
	require.Equal(t, []string{"uuid"}, readShape.Result[1].AllowedTypes)
	require.Equal(t, "created_at", readShape.Result[10].Name)

	// Verify embedded list row exposes all embedded fields plus Extra.
	listShape := qList.Definition().StatementShapes["page"]
	require.Len(t, listShape.Result, 12)
	require.Equal(t, "scope_id", listShape.Result[1].Name)
	require.Equal(t, []string{"uuid"}, listShape.Result[1].AllowedTypes)
	require.Equal(t, "created_at", listShape.Result[10].Name)
	require.Equal(t, "extra", listShape.Result[11].Name)
	require.Equal(t, []string{"uuid"}, qList.Definition().StatementShapes["keyset.scope.after"].Inputs[1].AllowedTypes)

	// Verify prefixed JOIN row exposes 22 distinct columns without cross-field role conflict.
	require.Len(t, qJoin.Definition().StatementShapes["sql"].Result, 22)
	require.Equal(t, "l_created_at", qJoin.Definition().StatementShapes["sql"].Result[10].Name)
	require.Equal(t, "r_created_at", qJoin.Definition().StatementShapes["sql"].Result[21].Name)

	// Verify runtime scan populates every field via ScopeWrap, json, zero, array, and embedded auditCols.
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	scopeVal := unitWrappedScope{raw: "11111111-1111-1111-1111-111111111111"}
	sess := &fakeAuthSession[unitWrappedScope]{
		scope:     scopeVal,
		authority: reg.SQLAuthority(),
		eachRows: [][]any{{
			int64(7),
			scopeVal.raw,
			"A-1",
			"Alpha",
			3,
			nil,
			nil,
			[]string{"red", "blue"},
			`{"k":"v"}`,
			"sec-1",
			now,
		}},
	}
	sess.proof = key.Sign(sess)

	got, err := qRead.Bind(sess).One(t.Context(), codeArgs{Code: "A-1"})
	require.NoError(t, err)
	require.Equal(t, sharedItem{
		ID:        7,
		ScopeID:   scopeVal,
		Code:      "A-1",
		Name:      "Alpha",
		Version:   3,
		DeletedAt: nil,
		Note:      "",
		Tags:      []string{"red", "blue"},
		Meta:      map[string]any{"k": "v"},
		Token:     "sec-1",
		auditCols: auditCols{CreatedAt: now},
	}, got)
	require.Equal(t, []any{scopeVal.raw, "A-1"}, sess.lastSQL.Args)

	retGot, err := qRet.Bind(sess).One(t.Context(), codeArgs{Code: "A-1"})
	require.NoError(t, err)
	require.Equal(t, got, retGot)

	sess.eachRows = [][]any{{
		int64(7),
		scopeVal.raw,
		"A-1",
		"Alpha",
		3,
		now,
		"hello",
		[]string{"red"},
		`{"n":1}`,
		"sec-2",
		now,
		"note-extra",
	}}
	listRows, err := qList.Bind(sess).All(t.Context(), tabula.NoArgs{}, tabula.QueryOrder{Sort: "id", Direction: tabula.SortAsc})
	require.NoError(t, err)
	require.Len(t, listRows, 1)
	require.Equal(t, scopeVal, listRows[0].ScopeID)
	require.NotNil(t, listRows[0].DeletedAt)
	require.Equal(t, now, *listRows[0].DeletedAt)
	require.NotNil(t, listRows[0].Extra)
	require.Equal(t, "note-extra", *listRows[0].Extra)

	// Verify invalid row tag combinations and unknown options are still rejected.
	type badUnknown struct {
		ID int64 `db:"id,bogus"`
	}
	_, err = tabula.TryDefineSQL[badUnknown, tabula.NoArgs](reg, tabula.SQLSpec{
		Name: "bad.unknown", SQL: "SELECT id FROM public.items", Scope: tabula.ScopeGlobal(),
	})
	require.ErrorContains(t, err, "option bogus is unknown")

	type badDupRole struct {
		ID int64 `db:"id,key,key"`
	}
	_, err = tabula.TryDefineSQL[badDupRole, tabula.NoArgs](reg, tabula.SQLSpec{
		Name: "bad.dup_role", SQL: "SELECT id FROM public.items", Scope: tabula.ScopeGlobal(),
	})
	require.ErrorContains(t, err, "cannot combine with")

	type badTwoRoles struct {
		ID int64 `db:"id,key,scope"`
	}
	_, err = tabula.TryDefineSQL[badTwoRoles, tabula.NoArgs](reg, tabula.SQLSpec{
		Name: "bad.two_roles", SQL: "SELECT id FROM public.items", Scope: tabula.ScopeGlobal(),
	})
	require.ErrorContains(t, err, "cannot combine with")

	type badRoleSecret struct {
		ID int64 `db:"id,key,secret"`
	}
	_, err = tabula.TryDefineSQL[badRoleSecret, tabula.NoArgs](reg, tabula.SQLSpec{
		Name: "bad.role_secret", SQL: "SELECT id FROM public.items", Scope: tabula.ScopeGlobal(),
	})
	require.ErrorContains(t, err, "cannot combine with")

	type badRoleJSON struct {
		Meta map[string]any `db:"meta,scope,json"`
	}
	_, err = tabula.TryDefineSQL[badRoleJSON, tabula.NoArgs](reg, tabula.SQLSpec{
		Name: "bad.role_json", SQL: "SELECT meta FROM public.items", Scope: tabula.ScopeGlobal(),
	})
	require.ErrorContains(t, err, "cannot combine with")

	type badSerialNoKey struct {
		ID int64 `db:"id,serial"`
	}
	_, err = tabula.TryDefineSQL[badSerialNoKey, tabula.NoArgs](reg, tabula.SQLSpec{
		Name: "bad.serial_no_key", SQL: "SELECT id FROM public.items", Scope: tabula.ScopeGlobal(),
	})
	require.ErrorContains(t, err, "serial requires an integer key column")

	type badSerialNonInt struct {
		ID string `db:"id,key,serial"`
	}
	_, err = tabula.TryDefineSQL[badSerialNonInt, tabula.NoArgs](reg, tabula.SQLSpec{
		Name: "bad.serial_str", SQL: "SELECT id FROM public.items", Scope: tabula.ScopeGlobal(),
	})
	require.ErrorContains(t, err, "serial requires an integer key column")

	type badVersionNonInt struct {
		Ver string `db:"ver,version"`
	}
	_, err = tabula.TryDefineSQL[badVersionNonInt, tabula.NoArgs](reg, tabula.SQLSpec{
		Name: "bad.ver_str", SQL: "SELECT ver FROM public.items", Scope: tabula.ScopeGlobal(),
	})
	require.ErrorContains(t, err, "version column must be an integer")

	type badJSONZero struct {
		Meta string `db:"meta,json,zero"`
	}
	_, err = tabula.TryDefineSQL[badJSONZero, tabula.NoArgs](reg, tabula.SQLSpec{
		Name: "bad.json_zero", SQL: "SELECT meta FROM public.items", Scope: tabula.ScopeGlobal(),
	})
	require.ErrorContains(t, err, "cannot combine with zero")

	type badPrefixed struct {
		Item badUnknown `db:"p_"`
	}
	_, err = tabula.TryDefineSQL[badPrefixed, tabula.NoArgs](reg, tabula.SQLSpec{
		Name: "bad.prefixed", SQL: "SELECT id AS p_id FROM public.items", Scope: tabula.ScopeGlobal(),
	})
	require.ErrorContains(t, err, "row column p_id: option bogus is unknown")

	// Verify compileSQLArgs still strictly rejects every table lifecycle option.
	type simpleRow struct {
		ID int64 `db:"id"`
	}
	for _, tc := range []struct {
		name string
		fn   func() error
	}{
		{"key", func() error {
			type a struct {
				ID int64 `db:"id,key"`
			}
			_, err := tabula.TryDefineSQL[simpleRow, a](reg, tabula.SQLSpec{Name: "arg.key", SQL: "SELECT id FROM public.items WHERE id = @id", Scope: tabula.ScopeGlobal()})
			return err
		}},
		{"scope", func() error {
			type a struct {
				ID int64 `db:"id,scope"`
			}
			_, err := tabula.TryDefineSQL[simpleRow, a](reg, tabula.SQLSpec{Name: "arg.scope", SQL: "SELECT id FROM public.items WHERE id = @id", Scope: tabula.ScopeGlobal()})
			return err
		}},
		{"deleted", func() error {
			type a struct {
				ID int64 `db:"id,deleted"`
			}
			_, err := tabula.TryDefineSQL[simpleRow, a](reg, tabula.SQLSpec{Name: "arg.deleted", SQL: "SELECT id FROM public.items WHERE id = @id", Scope: tabula.ScopeGlobal()})
			return err
		}},
		{"version", func() error {
			type a struct {
				ID int64 `db:"id,version"`
			}
			_, err := tabula.TryDefineSQL[simpleRow, a](reg, tabula.SQLSpec{Name: "arg.version", SQL: "SELECT id FROM public.items WHERE id = @id", Scope: tabula.ScopeGlobal()})
			return err
		}},
		{"once", func() error {
			type a struct {
				ID int64 `db:"id,once"`
			}
			_, err := tabula.TryDefineSQL[simpleRow, a](reg, tabula.SQLSpec{Name: "arg.once", SQL: "SELECT id FROM public.items WHERE id = @id", Scope: tabula.ScopeGlobal()})
			return err
		}},
		{"serial", func() error {
			type a struct {
				ID int64 `db:"id,serial"`
			}
			_, err := tabula.TryDefineSQL[simpleRow, a](reg, tabula.SQLSpec{Name: "arg.serial", SQL: "SELECT id FROM public.items WHERE id = @id", Scope: tabula.ScopeGlobal()})
			return err
		}},
	} {
		t.Run("args_rejects_"+tc.name, func(t *testing.T) {
			err := tc.fn()
			require.ErrorContains(t, err, `unsupported option "`+tc.name+`"`)
		})
	}
}

func FuzzDefineSQL(f *testing.F) {
	seeds := []string{
		"SELECT id FROM public.items WHERE id = @id",
		"SELECT id FROM public.items WHERE id = @id FOR UPDATE",
		"SELECT id FROM public.items WHERE id = $1",
		"SELECT id FROM public.items; DROP TABLE public.items",
		"SELECT set_config('demo.scope', '1', true) AS id WHERE 1 = @id",
		"SELECT id FROM public.items WHERE note = '@id' AND id = @id",
		"SELECT id FROM public.items WHERE tags <@ ARRAY['a'] AND id = @id",
		"WITH d AS (DELETE FROM public.items WHERE id = @id RETURNING id) SELECT id FROM d",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	type row struct {
		ID int64 `db:"id"`
	}
	type args struct {
		ID int64 `db:"id"`
	}
	f.Fuzz(func(t *testing.T, query string) {
		reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{
			Deny: tabula.DenyLegacySQL,
			SessionGuard: tabula.SessionGuardConfig{
				VariablePrefixes:    []string{"demo."},
				ForbidVariableReads: true,
			},
		})
		q, err := tabula.TryDefineSQL[row, args](reg, tabula.SQLSpec{
			Name: "fuzz.read",
			SQL:  query,
		})
		if err == nil {
			// Any admitted DefineSQL query must satisfy GuardNoSessionEscape and SingleDML.
			require.NoError(t, tabula.GuardNoSessionEscape(q.SQL()))
			require.NoError(t, tabula.GuardSingleStatement(q.SQL()))
			require.Zero(t, q.Definition().StatementShapes["sql"].Effects&tabula.EffectWrite)
		}
		_, _ = tabula.TryDefineSQLExec[args](reg, tabula.SQLSpec{Name: "fuzz.exec", SQL: query})
		_, _ = tabula.TryDefineSQLReturning[row, args](reg, tabula.SQLSpec{Name: "fuzz.ret", SQL: query})
	})
}
