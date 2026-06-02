package pgtest

import "go.uber.org/goleak"

// GoleakOptions returns the canonical goroutine-leak ignore list for the
// integration TestMains that wrap m.Run() with goleak.Find after RunMain.
// It covers library-managed background goroutines that legitimately
// outlive the test process and are not leaks. Pass extra options to
// append package-specific ignores.
//
// Ignored:
//   - testcontainers reaper connection goroutine
//   - pgxpool background health-check goroutine
//   - go-winio IO completion processor — the Docker client's Windows
//     named-pipe goroutine, created at package init and parked in a
//     syscall (so it is matched by stack frame, not top frame). Absent on
//     Linux, where the Docker client uses a Unix socket.
func GoleakOptions(extra ...goleak.Option) []goleak.Option {
	opts := []goleak.Option{
		goleak.IgnoreTopFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
		goleak.IgnoreAnyFunction("github.com/Microsoft/go-winio.ioCompletionProcessor"),
	}
	return append(opts, extra...)
}
