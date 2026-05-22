//go:build integration

package pg_test

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
		// Windows-only: Docker's named-pipe transport keeps an IOCP processor
		// goroutine alive across testcontainers tear-down. The actual top
		// frame is syscall.SyscallN (too generic to ignore by top-fn name)
		// — match anywhere in the stack instead. Harmless on Linux CI
		// (the goroutine doesn't exist there — the ignore is a no-op).
		goleak.IgnoreAnyFunction("github.com/Microsoft/go-winio.ioCompletionProcessor"),
	)
}
