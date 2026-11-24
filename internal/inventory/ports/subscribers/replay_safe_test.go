package subscribers_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/inventory/ports/subscribers"
)

// Review G2c B2, G2d B3: reserve is never replayed automatically (a late reserve on a
// cancelled order holds stock for good), so its dead letters wait for a person; there is no
// consume handler any more (ADR 0072). The release is replayed: cancellation is terminal and a release
// only drains what is still held. The policy replica opts in too (newer version only).
func TestHandlers_OnlyThePolicyReplicaAndTheReleaseAreReplaySafe(t *testing.T) {
	t.Parallel()
	handlers := subscribers.Handlers(
		&subscribers.ReserveStockSubscriber{}, &subscribers.ReleaseStockSubscriber{}, &subscribers.PolicyChangedSubscriber{})

	require.ElementsMatch(t,
		[]string{subscribers.HandlerPolicyChanged, subscribers.HandlerPolicyRepublished, subscribers.HandlerReleaseStock},
		messaging.ReplaySafeHandlerNames(handlers))
}
