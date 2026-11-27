package sqlcheck

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

type verificationBackend func(context.Context) (VerificationTx, error)

func (f verificationBackend) BeginVerification(ctx context.Context) (VerificationTx, error) {
	return f(ctx)
}

type verificationProbe struct {
	exec     func(context.Context, string, ...any) error
	scan     func(context.Context, string, []any, ...any) error
	describe func(context.Context, string, string) (Description, error)
	rollback func(context.Context) error
}

func (p verificationProbe) Exec(ctx context.Context, query string, args ...any) error {
	return p.exec(ctx, query, args...)
}
func (p verificationProbe) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	return p.scan(ctx, query, args, dest...)
}
func (verificationProbe) Each(context.Context, string, []any, func(func(...any) error) error) error {
	return errors.New("unexpected catalog row query")
}
func (p verificationProbe) Describe(ctx context.Context, name, query string) (Description, error) {
	return p.describe(ctx, name, query)
}
func (p verificationProbe) Rollback(ctx context.Context) error { return p.rollback(ctx) }

func TestVerifierRollbackAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	primary, cleanup := errors.New("verification failed"), errors.New("rollback failed")
	called := false
	tx := verificationProbe{rollback: func(ctx context.Context) error {
		called = true
		require.NoError(t, ctx.Err())
		_, bounded := ctx.Deadline()
		require.True(t, bounded)
		return cleanup
	}}
	backend := verificationBackend(func(context.Context) (VerificationTx, error) { return tx, nil })
	findings, err := verifyUsing(ctx, backend, func(VerificationTx) ([]string, error) {
		cancel()
		return []string{"items: invalid type"}, primary
	})
	require.True(t, called)
	require.Equal(t, []string{"items: invalid type"}, findings)
	require.ErrorIs(t, err, primary)
	require.ErrorIs(t, err, cleanup)
}

func TestVerifierCapabilityFailureStopsWorkload(t *testing.T) {
	const writeSQL = "INSERT INTO public.items(id) VALUES (1) RETURNING id"
	var commands []string
	descriptions, rolledBack := 0, false
	tx := verificationProbe{
		exec: func(_ context.Context, query string, _ ...any) error {
			commands = append(commands, query)
			return nil
		},
		scan: func(_ context.Context, query string, _ []any, dest ...any) error {
			switch {
			case strings.HasPrefix(query, "SELECT current_user"):
				*dest[0].(*string), *dest[1].(*string) = "scope_writer", "on"
			case strings.Contains(query, "pg_prepared_statements"):
				*dest[0].(*int) = 0
			default:
				t.Errorf("unexpected execution probe: %s", query)
			}
			return nil
		},
		describe: func(_ context.Context, _, query string) (Description, error) {
			descriptions++
			require.Equal(t, writeSQL, query)
			return Description{}, ErrDescribeRequired
		},
		rollback: func(context.Context) error { rolledBack = true; return nil },
	}
	backend := verificationBackend(func(context.Context) (VerificationTx, error) { return tx, nil })
	findings, err := VerifyStatementsUsing(t.Context(), backend, []tabula.Definition{{
		Name: "items", Statements: map[string]string{"a": writeSQL, "b": "SELECT 2"},
		StatementShapes: map[string]tabula.StatementShape{
			"a": {Effects: tabula.EffectWrite, Result: []tabula.ShapeField{{Name: "id", Type: "pg_catalog.int4"}}},
		},
	}}, "scope_writer")
	require.ErrorIs(t, err, ErrDescribeRequired)
	require.Empty(t, findings)
	require.Equal(t, 1, descriptions)
	require.True(t, rolledBack)
	require.Len(t, commands, 7)
	require.True(t, strings.HasPrefix(commands[4], "ROLLBACK TO SAVEPOINT "))
	require.True(t, strings.HasPrefix(commands[5], "DEALLOCATE "))
	require.True(t, strings.HasPrefix(commands[6], "RELEASE SAVEPOINT "))
}

func TestVerifierCleanupFailureIsFatal(t *testing.T) {
	cleanup := errors.New("savepoint cleanup failed")
	tx := verificationProbe{
		exec: func(_ context.Context, query string, _ ...any) error {
			if strings.HasPrefix(query, "RELEASE SAVEPOINT ") {
				return cleanup
			}
			return nil
		},
		scan: func(_ context.Context, query string, _ []any, dest ...any) error {
			switch {
			case strings.HasPrefix(query, "SELECT current_user"):
				*dest[0].(*string), *dest[1].(*string) = "scope_reader", "on"
			case strings.Contains(query, "pg_prepared_statements"):
				*dest[0].(*int) = 0
			default:
				*dest[0].(*string) = `[{"Plan":{"Node Type":"Result"}}]`
			}
			return nil
		},
		rollback: func(context.Context) error { return nil },
	}
	err := prepareUsingShape(t.Context(), tx, "SELECT 1", "scope_reader", nil)
	require.ErrorIs(t, err, cleanup)
	require.True(t, verificationFatal(err))
}

func TestVerifierRejectsMissingBackend(t *testing.T) {
	_, err := VerifyStatementsUsing(t.Context(), nil, nil, "")
	require.Error(t, err)
	backend := verificationBackend(func(context.Context) (VerificationTx, error) { return nil, nil })
	_, err = VerifyStatementsUsing(t.Context(), backend, nil, "")
	require.ErrorContains(t, err, "no transaction")
}
