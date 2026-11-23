package subscribers_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/orders/ports/subscribers"
)

// Review G2c B2: no order-lifecycle handler is replayed automatically. A late
// auto-invoice or consignment step applied to an order that has moved on is wrong; those
// dead letters wait for a person. Only the policy replica, which stores a newer version
// only, opts in.
func TestHandlers_OnlyThePolicyReplicaIsReplaySafe(t *testing.T) {
	t.Parallel()
	handlers := subscribers.Handlers(
		&subscribers.AutoInvoiceSubscriber{}, &subscribers.ConsignmentCreatedSubscriber{},
		&subscribers.ConsignmentDeliveredSubscriber{}, &subscribers.CancelCompensationSubscriber{},
		&subscribers.PolicyChangedSubscriber{})

	require.ElementsMatch(t,
		[]string{subscribers.HandlerPolicyChanged, subscribers.HandlerPolicyRepublished},
		messaging.ReplaySafeHandlerNames(handlers))
}
