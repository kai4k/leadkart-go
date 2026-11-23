package subscribers_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/dispatch/ports/subscribers"
)

// Review G2c B2: a consignment note created late for an order that was cancelled is a
// pending note on a cancelled order. No Dispatch handler is replayed automatically.
func TestHandlers_NoDispatchHandlerIsReplaySafe(t *testing.T) {
	t.Parallel()
	handlers := subscribers.Handlers(&subscribers.OrderPackedIngestor{}, &subscribers.OrderCancelledSubscriber{})

	require.Empty(t, messaging.ReplaySafeHandlerNames(handlers))
	require.Len(t, handlers, 2)
}
