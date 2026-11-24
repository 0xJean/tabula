package stdsql_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/stdsql"
	"github.com/stretchr/testify/require"
)

func TestNewRunnerValidation(t *testing.T) {
	db := new(sql.DB)

	_, err := stdsql.NewRunner[string](nil, stdsql.ScopePolicy[string]{}, stdsql.RunnerOptions{})
	require.Error(t, err)

	_, err = stdsql.NewRunner(db, stdsql.ScopePolicy[string]{}, stdsql.RunnerOptions{RequireRole: true})
	require.ErrorContains(t, err, "RequireRole")

	_, err = stdsql.NewRunner(db, stdsql.ScopePolicy[string]{}, stdsql.RunnerOptions{Role: " spaced "})
	require.ErrorContains(t, err, "invalid role")

	_, err = stdsql.NewRunner(db, stdsql.ScopePolicy[string]{}, stdsql.RunnerOptions{Unscoped: stdsql.UnscopedMode(99)})
	require.ErrorContains(t, err, "invalid Unscoped")

	_, err = stdsql.NewRunner(db, stdsql.ScopePolicy[string]{}, stdsql.RunnerOptions{
		ReadOnly: true,
		Unscoped: stdsql.UnscopedReadWrite,
	})
	require.ErrorContains(t, err, "ReadOnly")

	for _, opts := range []stdsql.RunnerOptions{
		{Role: "writer", UnscopedReadRole: "reader"},
		{Unscoped: stdsql.UnscopedReadOnly, UnscopedReadRole: "reader"},
		{Role: "writer", Unscoped: stdsql.UnscopedReadOnly, UnscopedReadRole: "writer"},
		{Role: "writer", Unscoped: stdsql.UnscopedReadOnly, UnscopedReadRole: " reader "},
		{Role: "writer", Unscoped: stdsql.UnscopedReadOnly, UnscopedReadRole: "none"},
	} {
		_, err = stdsql.NewRunner(db, stdsql.ScopePolicy[string]{}, opts)
		require.Error(t, err)
	}
	for _, bad := range []stdsql.Timeouts{
		{Lock: -time.Second},
		{Statement: 500 * time.Microsecond},
		{IdleInTransaction: 100 * time.Microsecond},
		{Transaction: -time.Second},
		{Lock: 5 * time.Second, Statement: 2 * time.Second},
		{Statement: 5 * time.Second, Transaction: 2 * time.Second},
	} {
		_, err = stdsql.NewRunner(db, stdsql.ScopePolicy[string]{}, stdsql.RunnerOptions{Timeouts: bad})
		require.Error(t, err, "expected timeout validation error for %+v", bad)
	}

	for _, badVar := range []string{"", "nodot", ".lead", "trail.", "a..b", "a.b;drop", "lock_timeout", "statement_timeout", "role"} {
		_, err = stdsql.NewRunner(db, stdsql.ScopePolicy[string]{
			ManagedVars: []string{badVar},
		}, stdsql.RunnerOptions{})
		require.Error(t, err, "expected managed var error for %q", badVar)
	}

	_, err = stdsql.NewRunner(db, stdsql.ScopePolicy[string]{
		ManagedVars: []string{"app.scope", "app.scope"},
	}, stdsql.RunnerOptions{})
	require.ErrorContains(t, err, "duplicate")

	_, err = stdsql.NewRunner(db, stdsql.ScopePolicy[string]{
		ManagedVars:  []string{"app.scope"},
		UnscopedVars: map[string]string{"app.other": "1"},
	}, stdsql.RunnerOptions{})
	require.ErrorContains(t, err, "not declared")

	_, err = stdsql.NewRunner(db, stdsql.ScopePolicy[string]{
		ManagedVars:  []string{"app.scope"},
		InactiveVars: map[string]string{"app.other": ""},
	}, stdsql.RunnerOptions{})
	require.ErrorContains(t, err, "not declared")

	r, err := stdsql.NewRunner(db, stdsql.ScopePolicy[string]{
		ManagedVars:  []string{"app.scope", "app.mode"},
		IsUnscoped:   func(s string) bool { return s == "all" },
		UnscopedVars: map[string]string{"app.mode": "all"},
		InactiveVars: map[string]string{"app.mode": "off"},
	}, stdsql.RunnerOptions{
		Role:        "app_ro",
		RequireRole: true,
		ReadOnly:    true,
		Unscoped:    stdsql.UnscopedReadOnly,
		Timeouts: stdsql.Timeouts{
			Lock:              time.Second,
			Statement:         2 * time.Second,
			IdleInTransaction: 5 * time.Second,
			Transaction:       10 * time.Second,
		},
	})
	require.NoError(t, err)

	// Pre-BEGIN checks must reject before touching db (which has no driver).
	ctx := context.Background()
	require.ErrorIs(t, r.Within(ctx, "t1", func(context.Context, *stdsql.Session[string]) error { return nil }), tabula.ErrReadOnly)
	require.ErrorIs(t, r.WithinIsolation(ctx, "t1", sql.LevelSerializable, func(context.Context, *stdsql.Session[string]) error { return nil }), tabula.ErrReadOnly)
	require.ErrorIs(t, r.WithinOptions(ctx, "t1", &sql.TxOptions{ReadOnly: false}, func(context.Context, *stdsql.Session[string]) error { return nil }), tabula.ErrReadOnly)
	require.ErrorIs(t, r.WithinRaw(ctx, "t1", func(context.Context, *stdsql.RawSession[string]) error { return nil }), tabula.ErrCapabilityDenied)
	require.ErrorIs(t, r.WithinSharedOptions(ctx, &sql.TxOptions{ReadOnly: false}, func(context.Context, *stdsql.Shared[string]) error { return nil }), tabula.ErrReadOnly)
	require.ErrorIs(t, r.WithinReadOnly(tabula.MarkTransaction(ctx), "t1", func(context.Context, *stdsql.Session[string]) error { return nil }), tabula.ErrNested)
	require.ErrorIs(t, r.WithinShared(tabula.MarkTransaction(ctx), func(context.Context, *stdsql.Shared[string]) error { return nil }), tabula.ErrNested)
	require.Error(t, r.Within(ctx, "t1", nil))
	require.Error(t, r.WithinRaw(ctx, "t1", nil))
	require.Error(t, r.WithinShared(ctx, nil))

	// Denied unscoped mode rejects before BEGIN.
	rDenied, err := stdsql.NewRunner(db, stdsql.ScopePolicy[string]{
		IsUnscoped: func(s string) bool { return s == "all" },
	}, stdsql.RunnerOptions{Unscoped: stdsql.UnscopedDenied})
	require.NoError(t, err)
	require.ErrorIs(t, rDenied.WithinReadOnly(ctx, "all", func(context.Context, *stdsql.Session[string]) error { return nil }), tabula.ErrCapabilityDenied)

	// Undeclared GUC returned by callback rejects before BEGIN.
	rBadCB, err := stdsql.NewRunner(db, stdsql.ScopePolicy[string]{
		ManagedVars: []string{"app.scope"},
		ScopeVars:   func(string) map[string]string { return map[string]string{"app.extra": "x"} },
	}, stdsql.RunnerOptions{})
	require.NoError(t, err)
	require.ErrorContains(t, rBadCB.Within(ctx, "t1", func(context.Context, *stdsql.Session[string]) error { return nil }), "undeclared GUC")

	// Duplicate GUC across ScopeVars and ContextVars rejects before BEGIN.
	rDupCB, err := stdsql.NewRunner(db, stdsql.ScopePolicy[string]{
		ManagedVars: []string{"app.scope"},
		ScopeVars:   func(s string) map[string]string { return map[string]string{"app.scope": s} },
		ContextVars: func(context.Context) map[string]string { return map[string]string{"app.scope": "override"} },
	}, stdsql.RunnerOptions{})
	require.NoError(t, err)
	require.ErrorContains(t, rDupCB.Within(ctx, "t1", func(context.Context, *stdsql.Session[string]) error { return nil }), "duplicate GUC")
}

func TestHelpersValidation(t *testing.T) {
	require.Panics(t, func() {
		stdsql.NewSession[string](nil, "t1", stdsql.ScopePolicy[string]{})
	})
	require.Error(t, stdsql.SetLocalConfig(context.Background(), nil, "", "v"))
	require.Error(t, stdsql.SetLocalRole(context.Background(), nil, ""))

	_, err := stdsql.Direct("postgres", "http://example.com/db", stdsql.NoScopePolicy())
	require.Error(t, err)
	_, err = stdsql.Direct("postgres", "postgres://user:pass@localhost/db", stdsql.ScopePolicy[string]{
		ScopeVars: func(s string) map[string]string { return map[string]string{"app.s": s} },
	})
	require.Error(t, err)
}
