package stdsql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
)

type sqlStateErr string

func (e sqlStateErr) Error() string    { return "pg " + string(e) }
func (e sqlStateErr) SQLState() string { return string(e) }

func TestIsRetryableAndRetry(t *testing.T) {
	require.True(t, IsRetryable(tabula.ErrSerialization))
	require.True(t, IsRetryable(tabula.ErrDeadlock))
	require.True(t, IsRetryable(sqlStateErr("40001")))
	require.True(t, IsRetryable(sqlStateErr("40P01")))
	require.False(t, IsRetryable(tabula.ErrUnique))
	require.False(t, IsRetryable(errors.New("other")))
	require.False(t, IsRetryable(nil))

	var sleeps []time.Duration
	attempts := 0
	err := Retry(t.Context(), RetryConfig{
		MaxAttempts: 4,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    25 * time.Millisecond,
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
	}, func(_ context.Context) error {
		attempts++
		if attempts < 3 {
			return sqlStateErr("40001")
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 3, attempts)
	require.Equal(t, []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}, sleeps)

	// Non-retryable error stops immediately without sleeping.
	sleeps = nil
	attempts = 0
	err = Retry(t.Context(), RetryConfig{
		MaxAttempts: 3,
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
	}, func(_ context.Context) error {
		attempts++
		return tabula.ErrUnique
	})
	require.ErrorIs(t, err, tabula.ErrUnique)
	require.Equal(t, 1, attempts)
	require.Empty(t, sleeps)

	// Exhausting MaxAttempts returns the final error.
	attempts = 0
	err = Retry(t.Context(), RetryConfig{
		MaxAttempts: 2,
		Sleep:       func(_ context.Context, _ time.Duration) error { return nil },
	}, func(_ context.Context) error {
		attempts++
		return tabula.ErrDeadlock
	})
	require.ErrorIs(t, err, tabula.ErrDeadlock)
	require.Equal(t, 2, attempts)

	// Canceled context stops immediately.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = Retry(ctx, RetryConfig{MaxAttempts: 3}, func(_ context.Context) error {
		t.Fatal("should not execute after cancel")
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestQuoteIdent(t *testing.T) {
	require.Equal(t, `"sp_1"`, quoteIdent("sp_1"))
	require.Equal(t, `"a""b"`, quoteIdent(`a"b`))
}
