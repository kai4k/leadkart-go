package subscribers

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	ordersevents "github.com/leadkart/leadkart-go/internal/orders/integrationevents"
)

// aggregateLines is the only non-trivial mapping in the reserve subscriber:
// it must sum quantities per product (a duplicate product across order lines
// reserves once against the shared available pool) while preserving
// first-seen order for deterministic FEFO planning.
func TestAggregateLines_SumsByProductPreservingOrder(t *testing.T) {
	t.Parallel()
	lines := aggregateLines([]ordersevents.OrderLineItem{
		{ProductID: "prod-a", Quantity: 5},
		{ProductID: "prod-b", Quantity: 3},
		{ProductID: "prod-a", Quantity: 7},
	})
	require.Len(t, lines, 2)
	require.Equal(t, product.ID("prod-a"), lines[0].ProductID)
	require.Equal(t, int64(12), lines[0].Quantity)
	require.Equal(t, product.ID("prod-b"), lines[1].ProductID)
	require.Equal(t, int64(3), lines[1].Quantity)
}

func TestAggregateLines_EmptyItemsYieldsNoLines(t *testing.T) {
	t.Parallel()
	require.Empty(t, aggregateLines(nil))
}

// Review B2: a pack event whose stock has expired dead-letters at once; other failures retry.
func TestConsumeError_ExpiredStockDeadLettersAtOnceOthersRetry(t *testing.T) {
	t.Parallel()
	require.True(t, messaging.IsNonRetryable(consumeError(fmt.Errorf("consume: %w", batch.ErrExpired))))
	require.False(t, messaging.IsNonRetryable(consumeError(errors.New("db down"))))
}
