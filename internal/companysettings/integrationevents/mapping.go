package integrationevents

import (
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
)

// ErrInvalidUUID is returned when a domain ID fails to parse as a UUID
// (impossible when aggregates validate at construction; fail-loud otherwise).
var ErrInvalidUUID = errors.New("companysettings integrationevents: invalid uuid")

// FromDomainEvent translates a domain event into its integration event.
// Panics on an UNKNOWN domain event type: a programmer error, fail-loud.
func FromDomainEvent(d any) (Event, error) {
	switch e := d.(type) {
	case commercesettings.PolicyChangedEvent:
		tid, err := uuid.Parse(e.TenantID.String())
		if err != nil {
			return nil, fmt.Errorf("%w: tenant_id=%q: %w", ErrInvalidUUID, e.TenantID, err)
		}
		return PolicyChangedV1{
			TenantIDClaim: tid, Version: e.Version, Policy: e.Policy,
			ChangedKeys: e.ChangedKeys, Reason: e.Reason,
			ChangedByMembershipID: e.Actor.MembershipID.String(),
			OccurredAtUTC:         e.At.UTC(),
		}, nil
	case commercesettings.PolicyRepublishedEvent:
		tid, err := uuid.Parse(e.TenantID.String())
		if err != nil {
			return nil, fmt.Errorf("%w: tenant_id=%q: %w", ErrInvalidUUID, e.TenantID, err)
		}
		return PolicyRepublishedV1{
			TenantIDClaim: tid, Version: e.Version, Policy: e.Policy, OccurredAtUTC: e.At.UTC(),
		}, nil
	default:
		panic(fmt.Sprintf("companysettings integrationevents: unmapped domain event %T", d))
	}
}
