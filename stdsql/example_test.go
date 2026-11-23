package stdsql_test

import (
	"context"
	"fmt"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/stdsql"
)

func ExampleRetry() {
	attempts := 0
	err := stdsql.Retry(context.Background(), stdsql.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		Sleep:       func(context.Context, time.Duration) error { return nil },
	}, func(context.Context) error {
		attempts++
		if attempts == 1 {
			return tabula.ErrSerialization
		}
		return nil
	})
	fmt.Println(attempts, err)
	// Output:
	// 2 <nil>
}
