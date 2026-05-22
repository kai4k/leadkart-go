//go:build integration

package audit_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain wraps the package's integration tests in goleak.VerifyTestMain
// so any goroutine leaked past test completion fails the run. Per ADR
// 0019: "goleak.VerifyTestMain in integration packages".
//
// IgnoreTopFunction allowlists the well-known long-lived goroutines that
// testcontainers + pgx spawn and that legitimately outlive a single
// test fixture (the Ryuk reaper, pgxpool's health-check). If a NEW
// long-lived goroutine appears, prefer fixing the leak over expanding
// this list.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	)
}
