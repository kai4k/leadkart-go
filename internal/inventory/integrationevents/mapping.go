package integrationevents

import (
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// FromDomainEvent translates ANY recognised Inventory domain event into
// its canonical integration event. Used by drainXEvents in the
// repository adapters: domain events emitted by aggregates flow through
// this function before they hit the outbox table.
//
// Returns [ErrUnknownDomainEvent] for events the mapper hasn't been
// taught about — surfaces in CI as "you minted a domain event but never
// wired the integration counterpart" failure.
func FromDomainEvent(d any) (Event, error) {
	switch e := d.(type) {
	case product.CreatedEvent:
		return ProductCreatedV1{
			ProductID:             mustParseUUID(e.ProductID.String()),
			TenantIDClaim:         mustParseUUID(e.TenantID.String()),
			SKU:                   e.SKU,
			Name:                  e.Name,
			CreatedAt:             e.At.UTC(),
			CreatedByMembershipID: mustParseUUID(e.ActorID.String()),
			OccurredAtUTC:         e.At.UTC(),
		}, nil

	case product.UpdatedEvent:
		return ProductUpdatedV1{
			ProductID:             mustParseUUID(e.ProductID.String()),
			TenantIDClaim:         mustParseUUID(e.TenantID.String()),
			ChangedFields:         append([]string(nil), e.ChangedFields...),
			UpdatedAt:             e.At.UTC(),
			UpdatedByMembershipID: mustParseUUID(e.ActorID.String()),
			OccurredAtUTC:         e.At.UTC(),
		}, nil

	case product.DeactivatedEvent:
		return ProductDeactivatedV1{
			ProductID:                 mustParseUUID(e.ProductID.String()),
			TenantIDClaim:             mustParseUUID(e.TenantID.String()),
			DeactivatedAt:             e.At.UTC(),
			DeactivatedByMembershipID: mustParseUUID(e.ActorID.String()),
			OccurredAtUTC:             e.At.UTC(),
		}, nil

	case batch.AddedEvent:
		return BatchAddedV1{
			BatchID:             mustParseUUID(e.BatchID.String()),
			ProductID:           mustParseUUID(e.ProductID.String()),
			TenantIDClaim:       mustParseUUID(e.TenantID.String()),
			BatchNumber:         e.BatchNumber,
			ExpiryDate:          e.ExpiryDate.UTC(),
			QuantityOnHand:      e.QuantityOnHand,
			AddedAt:             e.At.UTC(),
			AddedByMembershipID: mustParseUUID(e.ActorID.String()),
			OccurredAtUTC:       e.At.UTC(),
		}, nil

	case stockmovement.LoggedEvent:
		return StockMovementLoggedV1{
			MovementID:        mustParseUUID(e.MovementID.String()),
			BatchID:           mustParseUUID(e.BatchID.String()),
			ProductID:         mustParseUUID(e.ProductID.String()),
			TenantIDClaim:     mustParseUUID(e.TenantID.String()),
			Type:              string(e.Type),
			Quantity:          e.Quantity,
			NewQuantityOnHand: e.QuantityOnHandAfter,
			ActorMembershipID: mustParseUUID(e.ActorMembershipID.String()),
			SourceReference:   e.SourceReference,
			OccurredAtUTC:     e.At.UTC(),
		}, nil
	}

	return nil, fmt.Errorf("%w: %T", ErrUnknownDomainEvent, d)
}

// ErrUnknownDomainEvent surfaces when [FromDomainEvent] is handed a
// type the mapper hasn't been taught.
var ErrUnknownDomainEvent = errors.New("inventory.integrationevents: unknown domain event type")

// mustParseUUID panics on a malformed UUID string. Domain IDs are
// minted via [ids.NewV7] which produces canonical RFC 9562 UUIDs; a
// parse failure here means the aggregate constructed an ID via a
// non-canonical path (programmer error) — fail-fast per
// `coding-standards.md` "Result vs exceptions" carve-out.
func mustParseUUID(s string) uuid.UUID {
	u, err := uuid.Parse(s)
	if err != nil {
		panic(fmt.Sprintf("inventory.integrationevents: malformed UUID %q: %v", s, err))
	}
	return u
}
