package subscribers_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/inventory/ports/subscribers"
)

// Review G2c B2: reserve, consume and release are never replayed automatically. A late
// reserve on a cancelled order holds stock for good; a late consume or release moves it
// wrongly. Those dead letters wait for a person. Only the policy replica opts in.
func TestHandlers_OnlyThePolicyReplicaIsReplaySafe(t *testing.T) {
	t.Parallel()
	handlers := subscribers.Handlers(
		&subscribers.ReserveStockSubscriber{}, &subscribers.ConsumeStockSubscriber{},
		&subscribers.ReleaseStockSubscriber{}, &subscribers.PolicyChangedSubscriber{})

	require.ElementsMatch(t,
		[]string{subscribers.HandlerPolicyChanged, subscribers.HandlerPolicyRepublished},
		messaging.ReplaySafeHandlerNames(handlers))
}
