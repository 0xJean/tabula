package pgxdriver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func TestSQLStateHelpers(t *testing.T) {
	require.Equal(t, "23505", tabula.SQLState(fmt.Errorf("wrap: %w", &pgconn.PgError{Code: "23505"})))
	require.Equal(t, "", tabula.SQLState(errors.New("plain")))
	require.True(t, tabula.IsUniqueViolation(&pgconn.PgError{Code: "23505"}))
	require.True(t, tabula.IsForeignKeyViolation(&pgconn.PgError{Code: "23503"}))
	require.True(t, tabula.IsCheckViolation(&pgconn.PgError{Code: "23514"}))
	require.True(t, tabula.IsNotNullViolation(&pgconn.PgError{Code: "23502"}))
	require.True(t, tabula.IsSerializationFailure(&pgconn.PgError{Code: "40001"}))
	require.True(t, tabula.IsDeadlock(&pgconn.PgError{Code: "40P01"}))
	require.True(t, tabula.IsQueryCanceled(&pgconn.PgError{Code: "57014"}))
	require.True(t, tabula.IsNumericOutOfRange(&pgconn.PgError{Code: "22003"}))
	require.True(t, tabula.IsRejectedValue(&pgconn.PgError{Code: "22P02"}))
	require.False(t, tabula.IsRejectedValue(&pgconn.PgError{Code: "23505"}))
}

func TestArrayColumn(t *testing.T) {
	wrapPlain, err := ArrayColumn(reflect.TypeFor[[]string]())
	require.NoError(t, err)
	var plain []string
	require.Equal(t, &plain, wrapPlain(&plain))

	type tag string
	wrap, err := ArrayColumn(reflect.TypeFor[[]tag]())
	require.NoError(t, err)
	tm := pgtype.NewMap()
	want := []tag{"a", "b"}
	buf, err := tm.Encode(pgtype.TextArrayOID, pgtype.BinaryFormatCode, wrap(&want), nil)
	require.NoError(t, err)
	var got []tag
	require.NoError(t, tm.Scan(pgtype.TextArrayOID, pgtype.BinaryFormatCode, buf, wrap(&got)))
	require.Equal(t, want, got)
	_, err = ArrayColumn(reflect.TypeFor[int]())
	require.ErrorContains(t, err, "slice type")
	_, err = ArrayColumn(reflect.TypeFor[[]int16]())
	require.ErrorContains(t, err, "unsupported array element type")
}

func TestNewRunnerValidationAndPreBeginChecks(t *testing.T) {
	pool := new(pgxpool.Pool)

	_, err := NewRunner[string](nil, ScopePolicy[string]{}, RunnerOptions{})
	require.Error(t, err)

	_, err = NewRunner(pool, ScopePolicy[string]{}, RunnerOptions{RequireRole: true})
	require.ErrorContains(t, err, "RequireRole")

	_, err = NewRunner(pool, ScopePolicy[string]{}, RunnerOptions{Role: " bad "})
	require.ErrorContains(t, err, "invalid role")

	_, err = NewRunner(pool, ScopePolicy[string]{}, RunnerOptions{Unscoped: UnscopedMode(99)})
	require.ErrorContains(t, err, "invalid Unscoped")

	_, err = NewRunner(pool, ScopePolicy[string]{}, RunnerOptions{ReadOnly: true, Unscoped: UnscopedReadWrite})
	require.ErrorContains(t, err, "ReadOnly")

	for _, opts := range []RunnerOptions{
		{Role: "writer", UnscopedReadRole: "reader"},
		{Unscoped: UnscopedReadOnly, UnscopedReadRole: "reader"},
		{Role: "writer", Unscoped: UnscopedReadOnly, UnscopedReadRole: "writer"},
		{Role: "writer", Unscoped: UnscopedReadOnly, UnscopedReadRole: " reader "},
		{Role: "writer", Unscoped: UnscopedReadOnly, UnscopedReadRole: "none"},
	} {
		_, err = NewRunner(pool, ScopePolicy[string]{}, opts)
		require.Error(t, err)
	}
	for _, bad := range []Timeouts{
		{Lock: -time.Second},
		{Statement: 500 * time.Microsecond},
		{IdleInTransaction: 100 * time.Microsecond},
		{Transaction: -time.Second},
		{Lock: 5 * time.Second, Statement: 2 * time.Second},
		{Statement: 5 * time.Second, Transaction: 2 * time.Second},
	} {
		_, err = NewRunner(pool, ScopePolicy[string]{}, RunnerOptions{Timeouts: bad})
		require.Error(t, err)
	}

	for _, badVar := range []string{"", "nodot", ".lead", "trail.", "a..b", "a.b;drop", "lock_timeout", "statement_timeout", "role"} {
		_, err = NewRunner(pool, ScopePolicy[string]{ManagedVars: []string{badVar}}, RunnerOptions{})
		require.Error(t, err)
	}

	_, err = NewRunner(pool, ScopePolicy[string]{ManagedVars: []string{"demo.s", "demo.s"}}, RunnerOptions{})
	require.ErrorContains(t, err, "duplicate")

	_, err = NewRunner(pool, ScopePolicy[string]{
		ManagedVars:  []string{"demo.s"},
		UnscopedVars: map[string]string{"demo.other": "1"},
	}, RunnerOptions{})
	require.ErrorContains(t, err, "not declared")

	_, err = NewRunner(pool, ScopePolicy[string]{
		ManagedVars:  []string{"demo.s"},
		InactiveVars: map[string]string{"demo.other": ""},
	}, RunnerOptions{})
	require.ErrorContains(t, err, "not declared")

	r, err := NewRunner(pool, ScopePolicy[string]{
		ManagedVars:  []string{"demo.s", "demo.mode"},
		IsUnscoped:   func(s string) bool { return s == "all" },
		UnscopedVars: map[string]string{"demo.mode": "all"},
		InactiveVars: map[string]string{"demo.mode": "off"},
	}, RunnerOptions{
		Role:        "scope_reader",
		RequireRole: true,
		ReadOnly:    true,
		Unscoped:    UnscopedReadOnly,
		Timeouts: Timeouts{
			Lock:              time.Second,
			Statement:         2 * time.Second,
			IdleInTransaction: 5 * time.Second,
			Transaction:       10 * time.Second,
		},
	})
	require.NoError(t, err)

	ctx := context.Background()
	require.ErrorIs(t, r.Within(ctx, "t1", func(context.Context, *Session[string]) error { return nil }), tabula.ErrReadOnly)
	require.ErrorIs(t, r.WithinTxOptions(ctx, "t1", pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(context.Context, *Session[string]) error { return nil }), tabula.ErrReadOnly)
	require.ErrorIs(t, r.WithinRaw(ctx, "t1", func(context.Context, *RawSession[string]) error { return nil }), tabula.ErrCapabilityDenied)
	require.ErrorIs(t, r.WithinSharedOptions(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(context.Context, *Shared[string]) error { return nil }), tabula.ErrReadOnly)
	require.ErrorIs(t, r.WithinReadOnly(tabula.MarkTransaction(ctx), "t1", func(context.Context, *Session[string]) error { return nil }), tabula.ErrNested)
	require.ErrorIs(t, r.WithinShared(tabula.MarkTransaction(ctx), func(context.Context, *Shared[string]) error { return nil }), tabula.ErrNested)
	require.Error(t, r.Within(ctx, "t1", nil))
	require.Error(t, r.WithinRaw(ctx, "t1", nil))
	require.Error(t, r.WithinShared(ctx, nil))

	rDenied, err := NewRunner(pool, ScopePolicy[string]{
		IsUnscoped: func(s string) bool { return s == "all" },
	}, RunnerOptions{Unscoped: UnscopedDenied})
	require.NoError(t, err)
	require.ErrorIs(t, rDenied.WithinReadOnly(ctx, "all", func(context.Context, *Session[string]) error { return nil }), tabula.ErrCapabilityDenied)

	rBadCB, err := NewRunner(pool, ScopePolicy[string]{
		ManagedVars: []string{"demo.s"},
		ScopeVars:   func(string) map[string]string { return map[string]string{"demo.extra": "1"} },
	}, RunnerOptions{})
	require.NoError(t, err)
	require.ErrorContains(t, rBadCB.Within(ctx, "t1", func(context.Context, *Session[string]) error { return nil }), "undeclared GUC")

	require.Panics(t, func() { NewSession[string](nil, "t1", ScopePolicy[string]{}) })
	require.Error(t, SetLocalConfig(ctx, nil, "", "v"))
	require.Error(t, SetLocalRole(ctx, nil, ""))
}
