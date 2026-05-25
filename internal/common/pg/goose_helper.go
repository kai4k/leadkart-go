package pg

import (
	"sync"

	"github.com/pressly/goose/v3"
)

// goose.SetDialect mutates a goose-internal package-global. When
// integration tests run with t.Parallel() (per TD1 discipline),
// concurrent callers race on that global. Wrap in sync.Once so the
// dialect is set exactly once per process — idempotent + race-free
// regardless of caller count.

var (
	setDialectOnce sync.Once
	setDialectErr  error
)

// EnsureGooseDialect ensures goose.SetDialect("postgres") has been
// called exactly once for the current process. Safe to call from any
// number of concurrent goroutines / parallel tests; the first caller
// performs the SetDialect, subsequent callers return the cached
// result.
//
// All goose-using sites (production cmd/migrate + every integration
// test that runs migrations against testcontainers Postgres) should
// call this helper instead of goose.SetDialect directly. Closes the
// race the Go scheduler exposes when N parallel integration tests
// each spin up their own testcontainers + run goose migrations
// concurrently.
func EnsureGooseDialect() error {
	setDialectOnce.Do(func() {
		setDialectErr = goose.SetDialect("postgres")
	})
	return setDialectErr
}
