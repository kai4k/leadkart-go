package integrationevents

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
)

// ErrInvalidUUID is the sentinel returned when a domain ID string
// fails to parse as a UUID. Normally IMPOSSIBLE because aggregates
// validate IDs at construction time — surfaces only if validation is
// bypassed (programmer error).
var ErrInvalidUUID = errors.New("dispatch integrationevents: invalid uuid")

// FromDomainEvent translates a recognised Dispatch domain event into
// its canonical integration event. Used by the repository adapter's
// drain helper — domain events emitted by the aggregate flow through
// here BEFORE they hit the dispatch.outbox table.
//
// The two domain events ([consignmentnote.CreatedEvent],
// [consignmentnote.StatusChangedEvent]) fan out into FIVE integration
// events because StatusChanged carries a (PriorStatus, NewStatus)
// pair and each transition deserves its own wire alias for clean
// subscriber routing.
//
// Panics on UNKNOWN domain event type — programmer error, fail-loud
// (mirror of crm integrationevents.FromDomainEvent rationale).
func FromDomainEvent(d any) (Event, error) {
	switch e := d.(type) {

	case consignmentnote.CreatedEvent:
		cnID, err := parseUUID("consignment_note_id", e.ConsignmentNoteID.String())
		if err != nil {
			return nil, err
		}
		tID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		oID, err := parseUUID("order_id", e.OrderID.String())
		if err != nil {
			return nil, err
		}
		createdBy, err := parseUUID("created_by_membership_id", e.CreatedByMembershipID.String())
		if err != nil {
			return nil, err
		}
		var eta *time.Time
		if e.ExpectedDeliveryAt != nil {
			t := e.ExpectedDeliveryAt.UTC()
			eta = &t
		}
		return ConsignmentNoteCreatedV1{
			ConsignmentNoteID:     cnID,
			TenantIDClaim:         tID,
			OrderID:               oID,
			CarrierName:           e.CarrierName,
			BoxCount:              e.BoxCount,
			WeightGrams:           e.WeightGrams,
			ExpectedDeliveryAtUTC: eta,
			CreatedByMembershipID: createdBy,
			OccurredAtUTC:         e.CreatedAt.UTC(),
		}, nil

	case consignmentnote.StatusChangedEvent:
		cnID, err := parseUUID("consignment_note_id", e.ConsignmentNoteID.String())
		if err != nil {
			return nil, err
		}
		tID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		oID, err := parseUUID("order_id", e.OrderID.String())
		if err != nil {
			return nil, err
		}
		actor, err := parseUUID("transitioned_by_membership", e.TransitionedByMembership.String())
		if err != nil {
			return nil, err
		}
		// Fan out by NewStatus — each terminal/transition has its own
		// wire alias so subscribers route on the alias they care about
		// rather than parsing the StatusChanged envelope.
		switch e.NewStatus {
		case consignmentnote.StatusDispatched:
			// We don't carry docket_number on StatusChangedEvent
			// (it's stored on the aggregate, not the event). The adapter
			// re-reads the aggregate's DocketNumber() when persisting.
			// In this pure-domain layer mapping we surface empty + a
			// follow-up integration-side enrichment occurs in the
			// adapter (B.3 work).
			return ConsignmentNoteDispatchedV1{
				ConsignmentNoteID:        cnID,
				TenantIDClaim:            tID,
				OrderID:                  oID,
				DocketNumber:             "",
				TransitionedByMembership: actor,
				OccurredAtUTC:            e.TransitionedAt.UTC(),
			}, nil
		case consignmentnote.StatusInTransit:
			return ConsignmentNoteInTransitV1{
				ConsignmentNoteID:        cnID,
				TenantIDClaim:            tID,
				OrderID:                  oID,
				TransitionedByMembership: actor,
				OccurredAtUTC:            e.TransitionedAt.UTC(),
			}, nil
		case consignmentnote.StatusDelivered:
			return ConsignmentDeliveredV1{
				ConsignmentNoteID:        cnID,
				TenantIDClaim:            tID,
				OrderID:                  oID,
				DeliveredAtUTC:           e.TransitionedAt.UTC(),
				TransitionedByMembership: actor,
				OccurredAtUTC:            e.TransitionedAt.UTC(),
			}, nil
		case consignmentnote.StatusFailed:
			return ConsignmentNoteFailedV1{
				ConsignmentNoteID:        cnID,
				TenantIDClaim:            tID,
				OrderID:                  oID,
				Reason:                   "", // enriched by adapter from the aggregate's FailureReason()
				FailedAtUTC:              e.TransitionedAt.UTC(),
				TransitionedByMembership: actor,
				OccurredAtUTC:            e.TransitionedAt.UTC(),
			}, nil
		default:
			return nil, fmt.Errorf("dispatch integrationevents: unmapped status transition %s", e.NewStatus)
		}

	default:
		panic(fmt.Sprintf("dispatch integrationevents: unmapped domain event %T", d))
	}
}

// parseUUID parses + returns ErrInvalidUUID on failure.
func parseUUID(name, s string) (uuid.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %s=%q: %w", ErrInvalidUUID, name, s, err)
	}
	return u, nil
}
