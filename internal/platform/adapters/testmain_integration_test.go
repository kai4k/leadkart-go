//go:build integration

package adapters_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain isolates pgxpool/testcontainers leak-noise from the
// goroutine checks per ADR 0019 + identity's pattern.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	)
}
