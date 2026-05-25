//go:build integration

// fixture_integration_test.go — TestMain + shared pool for the
// common/messaging integration test package.
//
// Inbox tests can run in parallel because each test uses a unique
// hardcoded message_id (11111111..., 22222222..., etc.) + unique
// handler names, so the (handler, message_id) dedup-key is naturally
// partitioned per-test.

package messaging_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"

	"github.com/leadkart/leadkart-go/internal/common/pgtest"
)

// sharedPG is the per-package Postgres container + app-role pool.
//
// nolint:gochecknoglobals // canonical TestMain shared-fixture pattern
var sharedPG *pgtest.Container

// TestMain bootstrap. messaging.inbox lives in the `app` schema; no
// tenant-scoped tables touched by these tests.
func TestMain(m *testing.M) {
	c, code := pgtest.RunMain(m, pgtest.Config{
		Schemas: []string{"identity"},
		Grants:  []string{"identity", "app"},
	})
	sharedPG = c

	if err := goleak.Find(
		goleak.IgnoreTopFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
		goleak.IgnoreTopFunction("github.com/jackc/pgx/v5/pgxpool.(*Pool).backgroundHealthCheck"),
	); err != nil {
		fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// inboxFixture returns the shared package-scoped Postgres pool.
// Signature preserved so existing test bodies don't change.
func inboxFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sharedPG.Pool()
}
