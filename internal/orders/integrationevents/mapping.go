package integrationevents

import (
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// ErrInvalidUUID is returned when a domain ID string fails to parse as a UUID.
// Normally IMPOSSIBLE — aggregates validate IDs at construction — so this
// surfaces only if that validation is bypassed (programmer error).
var ErrInvalidUUID = errors.New("orders integrationevents: invalid uuid")

// FromDomainEvent translates a recognised Orders domain event into its
// canonical integration event, or (nil, nil) to SUPPRESS — the latter for
// events whose wire counterpart carries derived data the bare domain event
// cannot (OrderConfirmedV1 needs the line snapshot, OrderPackedV1 the carrier
// logistics); those are published directly by their command via the
// OutboxEnqueuer. The repository drain calls this for every pulled event.
//
// Panics on an UNKNOWN domain-event type — fail-loud (mirror of dispatch /
// platform): a new domain event must be taught here before it can ship.
func FromDomainEvent(d any) (Event, error) {
	switch e := d.(type) {

	// ----- Order ------------------------------------------------------------
	case order.CancelledEvent:
		oID, err := parseUUID("order_id", e.OrderID.String())
		if err != nil {
			return nil, err
		}
		tID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		actor, err := parseUUID("cancelled_by_membership_id", e.CancelledByMembership.String())
		if err != nil {
			return nil, err
		}
		return OrderCancelledV1{
			OrderID:               oID.String(),
			TenantID:              tID.String(),
			PriorState:            e.PriorState.String(),
			Reason:                e.Reason,
			CancelledAtUTC:        e.CancelledAt.UTC(),
			CancelledByMembership: actor.String(),
		}, nil

	// CreatedEvent + AdvancedEvent carry no cross-module payload on their own —
	// the enriched OrderConfirmedV1 / OrderPackedV1 are published by the
	// confirming / packing commands. Suppress the bare events.
	case order.CreatedEvent, order.AdvancedEvent:
		return nil, nil //nolint:nilnil // intentional suppression — enriched events (OrderConfirmedV1/OrderPackedV1) emitted directly by the command

	// ----- Quotation --------------------------------------------------------
	// Quotation lifecycle is intra-module (no cross-module consumer yet) — the
	// Order side reads the approved quotation directly at Order creation.
	case quotation.CreatedEvent, quotation.RevisedEvent,
		quotation.ApprovedEvent, quotation.RejectedEvent:
		return nil, nil //nolint:nilnil // intentional suppression — quotation lifecycle is intra-module

	default:
		panic(fmt.Sprintf("orders integrationevents: unmapped domain event %T", d))
	}
}

// parseUUID parses s, returning ErrInvalidUUID on failure.
func parseUUID(name, s string) (uuid.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %s=%q: %w", ErrInvalidUUID, name, s, err)
	}
	return u, nil
}
