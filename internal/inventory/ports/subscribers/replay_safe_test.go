package subscribers_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/inventory/ports/subscribers"
)

// Review G2c B2, G2d B3: reserve and consume are never replayed automatically (a late reserve
// on a cancelled order holds stock for good; a late consume moves it wrongly), so those dead
// letters wait for a person. The release is replayed: cancellation is terminal and a release
// only drains what is still held. The policy replica opts in too (newer version only).
func TestHandlers_OnlyThePolicyReplicaAndTheReleaseAreReplaySafe(t *testing.T) {
	t.Parallel()
	handlers := subscribers.Handlers(
		&subscribers.ReserveStockSubscriber{}, &subscribers.ConsumeStockSubscriber{},
		&subscribers.ReleaseStockSubscriber{}, &subscribers.PolicyChangedSubscriber{})

	require.ElementsMatch(t,
		[]string{subscribers.HandlerPolicyChanged, subscribers.HandlerPolicyRepublished, subscribers.HandlerReleaseStock},
		messaging.ReplaySafeHandlerNames(handlers))
}
