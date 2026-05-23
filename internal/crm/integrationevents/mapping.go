package integrationevents

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

// FromDomainEvent translates ANY recognised CRM domain event into its
// canonical integration event. Used by the repository adapters' drain
// helpers: domain events emitted by aggregates flow through this
// function before they hit the crm.outbox table.
//
// Returns [ErrUnknownDomainEvent] for events the mapper hasn't been
// taught about — surfaces in CI as a clear "you minted a domain event
// but never wired the integration counterpart" failure.
//
//nolint:cyclop // Switch dispatcher — one case per recognised domain
// event. Cyclomatic complexity scales with catalogue size by definition;
// refactoring into a registry map costs more than it pays.
func FromDomainEvent(d any) (Event, error) {
	switch e := d.(type) {

	// ----- crmlead aggregate -----------------------------------------

	case crmlead.CreatedEvent:
		var purchase uuid.UUID
		if e.SourcePurchaseID != "" {
			purchase = mustParseUUID(e.SourcePurchaseID)
		}
		var createdBy uuid.UUID
		if e.CreatedByMembershipID != "" {
			createdBy = mustParseUUID(e.CreatedByMembershipID)
		}
		return CrmLeadCreatedV1{
			LeadID:                mustParseUUID(e.LeadID.String()),
			TenantIDClaim:         mustParseUUID(e.TenantID),
			SourcePurchaseID:      purchase,
			CreatedByMembershipID: createdBy,
			OccurredAtUTC:         e.At.UTC(),
		}, nil

	case crmlead.AssignedEvent:
		var prev uuid.UUID
		if e.PreviousAssignee != "" {
			prev = mustParseUUID(e.PreviousAssignee)
		}
		return CrmLeadAssignedV1{
			LeadID:                 mustParseUUID(e.LeadID.String()),
			TenantIDClaim:          mustParseUUID(e.TenantID),
			PreviousAssignee:       prev,
			AssigneeMembershipID:   mustParseUUID(e.AssigneeMembershipID),
			AssignedByMembershipID: mustParseUUID(e.AssignedByMembershipID),
			OccurredAtUTC:          e.At.UTC(),
		}, nil

	case crmlead.StageChangedEvent:
		return CrmLeadStageChangedV1{
			LeadID:                mustParseUUID(e.LeadID.String()),
			TenantIDClaim:         mustParseUUID(e.TenantID),
			OldStage:              e.OldStage.String(),
			NewStage:              e.NewStage.String(),
			ChangedByMembershipID: mustParseUUID(e.ChangedByMembershipID),
			Reason:                e.Reason,
			OccurredAtUTC:         e.At.UTC(),
		}, nil

	case crmlead.TemperatureChangedEvent:
		return CrmLeadTemperatureChangedV1{
			LeadID:                mustParseUUID(e.LeadID.String()),
			TenantIDClaim:         mustParseUUID(e.TenantID),
			OldTemperature:        e.OldTemperature.String(),
			NewTemperature:        e.NewTemperature.String(),
			ChangedByMembershipID: mustParseUUID(e.ChangedByMembershipID),
			OccurredAtUTC:         e.At.UTC(),
		}, nil

	case crmlead.ConvertedEvent:
		return CrmLeadConvertedV1{
			LeadID:                  mustParseUUID(e.LeadID.String()),
			TenantIDClaim:           mustParseUUID(e.TenantID),
			ConvertedByMembershipID: mustParseUUID(e.ConvertedByMembershipID),
			OccurredAtUTC:           e.At.UTC(),
		}, nil

	case crmlead.LostEvent:
		return CrmLeadLostV1{
			LeadID:             mustParseUUID(e.LeadID.String()),
			TenantIDClaim:      mustParseUUID(e.TenantID),
			LostByMembershipID: mustParseUUID(e.LostByMembershipID),
			Reason:             e.Reason,
			OccurredAtUTC:      e.At.UTC(),
		}, nil

	// ----- calllog aggregate -----------------------------------------

	case calllog.LoggedEvent:
		return CrmCallLoggedV1{
			CallID:               mustParseUUID(e.CallID.String()),
			LeadID:               mustParseUUID(e.LeadID.String()),
			TenantIDClaim:        mustParseUUID(e.TenantID),
			Outcome:              e.Outcome.String(),
			LoggedByMembershipID: mustParseUUID(e.LoggedByMembershipID),
			OccurredAtUTC:        e.At.UTC(),
		}, nil
	}

	return nil, fmt.Errorf("crm integrationevents: %w: %T", ErrUnknownDomainEvent, d)
}

// ErrUnknownDomainEvent surfaces when [FromDomainEvent] is handed a type
// the mapper hasn't been taught. CI surfaces as "you minted a domain
// event but the integration counterpart isn't wired".
var ErrUnknownDomainEvent = unknownErr("unknown crm domain event type")

type unknownErr string

func (u unknownErr) Error() string { return string(u) }

// mustParseUUID panics on a malformed UUID string. Domain IDs are
// minted via [ids.NewV7] which produces canonical RFC 9562 UUIDs; a
// parse failure here means the aggregate constructed an ID via a
// non-canonical path (programmer error) — fail-fast is the right
// response per coding-standards.md "Result vs exceptions".
func mustParseUUID(s string) uuid.UUID {
	u, err := uuid.Parse(s)
	if err != nil {
		panic(fmt.Sprintf("crm integrationevents: malformed UUID %q: %v", s, err))
	}
	return u
}
