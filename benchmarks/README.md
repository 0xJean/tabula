# Reading 100 rows from PostgreSQL

Each adapter reads the same three columns from the same 100 rows, ordered by primary key. The timed operation executes one parameterized SELECT, materializes the result in a fresh slice, and checks its length. A warm-up verifies every field. Schema creation, connection setup, descriptor compilation and result validation during warm-up are outside the timer.

All five adapters use pgx/v5 on the wire. Database/sql, sqlx and GORM share a database/sql pool using pgx/stdlib. Tabula uses its read-only Direct pool with the same driver. Native pgx uses pgxpool and CollectRows. Each adapter uses its normal mapping API. The benchmark module contains all comparison dependencies.

```sh
scripts/workspace.sh
cd benchmarks
TABULA_TEST_DSN='postgres://user:pass@localhost:5432/disposable?sslmode=disable' \
  go test -run='^$' -bench=BenchmarkSelect100 -benchmem -benchtime=1s -count=5
```

## Measurement

The table gives the median of five runs per adapter. Adapter lines count each formatted read function from its declaration through its closing brace, including error handling. The shared query, model and one-time setup are outside that count. [Implementations](select_test.go)

Apple M4 Pro, darwin/arm64, Go 1.27.4, PostgreSQL 15.19 in Docker over loopback. Each run used a one-second benchmark timer.

| Adapter | µs/op | B/op | allocs/op | Adapter lines |
| --- | ---: | ---: | ---: | ---: |
| database/sql | 312.4 | 16,933 | 408 | 16 |
| pgx | 288.5 | 18,827 | 316 | 7 |
| sqlx | 319.2 | 19,612 | 514 | 5 |
| GORM | 330.8 | 29,200 | 843 | 5 |
| Tabula | 310.2 | 19,451 | 529 | 3 |

The transaction and bulk-write benchmarks live in the [root module](../bench_integration_test.go) and the [pgxdriver module](../pgxdriver/integration_test.go).
