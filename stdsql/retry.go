package stdsql

import (
	"context"
	"errors"
	"time"

	"github.com/0xJean/tabula"
)

// RetryConfig controls explicit retries for transient PostgreSQL transaction
// conflicts (serialization failure 40001 and deadlock 40P01).
type RetryConfig struct {
	// MaxAttempts is the total number of attempts including the initial run
	// (defaults to 3 when <= 0).
	MaxAttempts int
	// BaseDelay is the initial exponential backoff delay (defaults to 25ms when <= 0).
	BaseDelay time.Duration
	// MaxDelay caps the backoff delay (defaults to 500ms when <= 0).
	MaxDelay time.Duration
	// ShouldRetry overrides retry eligibility (defaults to IsRetryable).
	ShouldRetry func(error) bool
	// Sleep overrides the delay wait (useful for deterministic unit tests).
	Sleep func(context.Context, time.Duration) error
}

// IsRetryable reports whether err is a PostgreSQL serialization failure
// (SQLSTATE 40001 / tabula.ErrSerialization) or deadlock (SQLSTATE 40P01 /
// tabula.ErrDeadlock).
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, tabula.ErrSerialization) || errors.Is(err, tabula.ErrDeadlock) {
		return true
	}
	switch tabula.SQLState(err) {
	case "40001", "40P01":
		return true
	}
	return false
}

// Retry invokes fn up to cfg.MaxAttempts times while fn returns a retryable
// error and ctx has not been canceled.
func Retry(ctx context.Context, cfg RetryConfig, fn func(context.Context) error) error {
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	delay := cfg.BaseDelay
	if delay <= 0 {
		delay = 25 * time.Millisecond
	}
	maxDelay := cfg.MaxDelay
	if maxDelay <= 0 {
		maxDelay = 500 * time.Millisecond
	}
	shouldRetry := cfg.ShouldRetry
	if shouldRetry == nil {
		shouldRetry = IsRetryable
	}
	sleepFn := cfg.Sleep
	if sleepFn == nil {
		sleepFn = defaultSleep
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == maxAttempts || !shouldRetry(err) {
			return err
		}
		wait := min(delay, maxDelay)
		if sleepErr := sleepFn(ctx, wait); sleepErr != nil {
			return sleepErr
		}
		delay = min(delay*2, maxDelay)
	}
	return lastErr
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
