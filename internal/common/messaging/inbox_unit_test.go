// inbox_unit_test.go — pure-Go unit tests for the IdempotentReceiver
// constructor + Wrap input validation. These tests do NOT require
// Postgres; they construct the receiver with `nil` pool and exercise
// only the in-process validation paths.
//
// Kept OUT of inbox_test.go (which is gated by the integration build
// constraint) because gating these on that tag forces a full pgtest
// container boot for a <1ms panic assertion. Per TDL canon §6 the
// integration tag is for SQL-contract tests only.

package messaging_test

import (
	"context"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
)

// TestIdempotentReceiver_Wrap_PanicsOnEmptyName asserts the
// constructor-time invariant that handler names are required. Pure-Go;
// no DB, no pool.
func TestIdempotentReceiver_Wrap_PanicsOnEmptyName(t *testing.T) {
	t.Parallel()
	receiver := messaging.NewIdempotentReceiver(nil) // pool not used in this path
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty handlerName")
		}
	}()
	_ = receiver.Wrap("", func(context.Context, string) error { return nil }) // arch-test:ignore-err — asserts panic; return value unreachable
}
