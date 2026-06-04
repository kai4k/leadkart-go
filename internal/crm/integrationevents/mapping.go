package integrationevents

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
)

// FromDomainEvent translates ANY recognised CRM domain event into its
// canonical integration event. Used by the repository adapters' drain
// helpers: domain events emitted by aggregates flow through this
// function before they hit the crm.outbox table.
//
// Panics on an UNKNOWN domain event type (default branch). Per
// reviewer H5: a domain event was minted by some aggregate without a
// matching integration counterpart here — programmer error, fail-loud
// at the boundary so the bug surfaces in tests + dev rather than as
// a silent drop in the request path. CI panic recovery in tests; in
// production the panic bubbles to the outbox writer which fails the
// request — Watermill retry + alert beats silent message loss.
//
// All UUID parsing is via [parseUUID] which returns an error rather
// than panicking (per reviewer H6) — the actual prevention happens at
// AGGREGATE CONSTRUCTION time (crmlead.New / NewFromPurchaseSnapshot /
// calllog.New validate every ID via uuid.Parse). The parseUUID error
// path here is the defense-in-depth in case validation is bypassed.
//
// event. Cyclomatic complexity scales with catalogue size by definition;
// refactoring into a registry map costs more than it pays.
//
//nolint:cyclop // Switch dispatcher — one case per recognised domain
func FromDomainEvent(d any) (Event, error) {
	switch e := d.(type) {

	// ----- crmlead aggregate -----------------------------------------

	case crmlead.CreatedEvent:
		leadID, err := parseUUID("lead_id", e.LeadID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		var purchase uuid.UUID
		if e.SourcePurchaseID != "" {
			purchase, err = parseUUID("source_purchase_id", e.SourcePurchaseID)
			if err != nil {
				return nil, err
			}
		}
		var createdBy uuid.UUID
		if e.CreatedByMembershipID != "" {
			createdBy, err = parseUUID("created_by_membership_id", e.CreatedByMembershipID)
			if err != nil {
				return nil, err
			}
		}
		return CrmLeadCreatedV1{
			LeadID:                leadID,
			TenantIDClaim:         tenantID,
			SourcePurchaseID:      purchase,
			CreatedByMembershipID: createdBy,
			OccurredAtUTC:         e.At.UTC(),
		}, nil

	case crmlead.AssignedEvent:
		leadID, err := parseUUID("lead_id", e.LeadID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		assignee, err := parseUUID("assignee_membership_id", e.AssigneeMembershipID)
		if err != nil {
			return nil, err
		}
		assignedBy, err := parseUUID("assigned_by_membership_id", e.AssignedByMembershipID)
		if err != nil {
			return nil, err
		}
		var prev uuid.UUID
		if e.PreviousAssignee != "" {
			prev, err = parseUUID("previous_assignee", e.PreviousAssignee)
			if err != nil {
				return nil, err
			}
		}
		return CrmLeadAssignedV1{
			LeadID:                 leadID,
			TenantIDClaim:          tenantID,
			PreviousAssignee:       prev,
			AssigneeMembershipID:   assignee,
			AssignedByMembershipID: assignedBy,
			OccurredAtUTC:          e.At.UTC(),
		}, nil

	case crmlead.StageChangedEvent:
		leadID, err := parseUUID("lead_id", e.LeadID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		changedBy, err := parseUUID("changed_by_membership_id", e.ChangedByMembershipID)
		if err != nil {
			return nil, err
		}
		return CrmLeadStageChangedV1{
			LeadID:                leadID,
			TenantIDClaim:         tenantID,
			OldStage:              e.OldStage.String(),
			NewStage:              e.NewStage.String(),
			ChangedByMembershipID: changedBy,
			Reason:                e.Reason,
			OccurredAtUTC:         e.At.UTC(),
		}, nil

	case crmlead.TemperatureChangedEvent:
		leadID, err := parseUUID("lead_id", e.LeadID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		changedBy, err := parseUUID("changed_by_membership_id", e.ChangedByMembershipID)
		if err != nil {
			return nil, err
		}
		return CrmLeadTemperatureChangedV1{
			LeadID:                leadID,
			TenantIDClaim:         tenantID,
			OldTemperature:        e.OldTemperature.String(),
			NewTemperature:        e.NewTemperature.String(),
			ChangedByMembershipID: changedBy,
			OccurredAtUTC:         e.At.UTC(),
		}, nil

	case crmlead.ConvertedEvent:
		leadID, err := parseUUID("lead_id", e.LeadID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		convertedBy, err := parseUUID("converted_by_membership_id", e.ConvertedByMembershipID)
		if err != nil {
			return nil, err
		}
		return CrmLeadConvertedV1{
			LeadID:                  leadID,
			TenantIDClaim:           tenantID,
			ConvertedByMembershipID: convertedBy,
			OccurredAtUTC:           e.At.UTC(),
		}, nil

	case crmlead.LostEvent:
		leadID, err := parseUUID("lead_id", e.LeadID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		lostBy, err := parseUUID("lost_by_membership_id", e.LostByMembershipID)
		if err != nil {
			return nil, err
		}
		return CrmLeadLostV1{
			LeadID:             leadID,
			TenantIDClaim:      tenantID,
			LostByMembershipID: lostBy,
			Reason:             e.Reason,
			OccurredAtUTC:      e.At.UTC(),
		}, nil

	// ----- calllog aggregate -----------------------------------------

	case calllog.LoggedEvent:
		callID, err := parseUUID("call_id", e.CallID.String())
		if err != nil {
			return nil, err
		}
		leadID, err := parseUUID("lead_id", e.LeadID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		loggedBy, err := parseUUID("logged_by_membership_id", e.LoggedByMembershipID)
		if err != nil {
			return nil, err
		}
		out := CrmCallLoggedV1{
			CallID:               callID,
			LeadID:               leadID,
			TenantIDClaim:        tenantID,
			Outcome:              e.Outcome.String(),
			LoggedByMembershipID: loggedBy,
			OccurredAtUTC:        e.At.UTC(),
		}
		// Per BRD §4.5: the CallbackWindow fields ride the V1 event as
		// additive omitzero values when the caller stamped a callback
		// window on the log_call request.
		if !e.CallbackWindowStart.IsZero() {
			out.CallbackWindowStartAt = e.CallbackWindowStart.UTC()
		}
		if !e.CallbackWindowEnd.IsZero() {
			out.CallbackWindowEndAt = e.CallbackWindowEnd.UTC()
		}
		return out, nil

	// ----- reminder aggregate ----------------------------------------

	case reminder.CreatedEvent:
		reminderID, err := parseUUID("reminder_id", e.ReminderID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		leadID, err := parseUUID("lead_id", e.LeadID.String())
		if err != nil {
			return nil, err
		}
		assignedTo, err := parseUUID("assigned_to_membership_id", e.AssignedToMembershipID)
		if err != nil {
			return nil, err
		}
		var sourceCall uuid.UUID
		if e.SourceCallLogID != "" {
			sourceCall, err = parseUUID("source_call_log_id", e.SourceCallLogID)
			if err != nil {
				return nil, err
			}
		}
		var createdBy uuid.UUID
		if e.CreatedByMembershipID != "" {
			createdBy, err = parseUUID("created_by_membership_id", e.CreatedByMembershipID)
			if err != nil {
				return nil, err
			}
		}
		return CrmReminderCreatedV1{
			ReminderID:             reminderID,
			TenantIDClaim:          tenantID,
			LeadID:                 leadID,
			AssignedToMembershipID: assignedTo,
			Type:                   e.Type.String(),
			DueAtUTC:               e.DueAt.UTC(),
			SourceCallLogID:        sourceCall,
			CreatedByMembershipID:  createdBy,
			OccurredAtUTC:          e.At.UTC(),
		}, nil

	case reminder.MarkedSentEvent:
		reminderID, err := parseUUID("reminder_id", e.ReminderID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		leadID, err := parseUUID("lead_id", e.LeadID.String())
		if err != nil {
			return nil, err
		}
		markedBy, err := parseUUID("marked_by_membership_id", e.MarkedByMembershipID)
		if err != nil {
			return nil, err
		}
		return CrmReminderMarkedSentV1{
			ReminderID:           reminderID,
			TenantIDClaim:        tenantID,
			LeadID:               leadID,
			MarkedByMembershipID: markedBy,
			OccurredAtUTC:        e.At.UTC(),
		}, nil

	case reminder.CancelledEvent:
		reminderID, err := parseUUID("reminder_id", e.ReminderID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		leadID, err := parseUUID("lead_id", e.LeadID.String())
		if err != nil {
			return nil, err
		}
		cancelledBy, err := parseUUID("cancelled_by_membership_id", e.CancelledByMembershipID)
		if err != nil {
			return nil, err
		}
		return CrmReminderCancelledV1{
			ReminderID:              reminderID,
			TenantIDClaim:           tenantID,
			LeadID:                  leadID,
			CancelledByMembershipID: cancelledBy,
			Reason:                  e.Reason,
			OccurredAtUTC:           e.At.UTC(),
		}, nil

	default:
		// Programmer error per reviewer H5 — a domain event has no
		// integration counterpart wired. Fail-loud: tests recover the
		// panic + report; production aborts the outbox write and the
		// retry/alert path takes over.
		panic(fmt.Sprintf("crm integrationevents: unmapped domain event %T", d))
	}
}

// parseUUID is the error-returning UUID parser used by FromDomainEvent.
// `name` is the field label embedded in the error message. Per
// reviewer H6: never panic from the request path on a parse failure —
// surface as ErrInvalidUUID so the outbox writer can map it back into
// a 5xx with a clear log line.
func parseUUID(name, s string) (uuid.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %s=%q: %w", ErrInvalidUUID, name, s, err)
	}
	return u, nil
}

// ErrInvalidUUID is the sentinel returned by [parseUUID] / FromDomainEvent
// when a domain ID string fails to parse as a RFC 9562 UUID. Normally
// IMPOSSIBLE because aggregates validate at construction time
// (crmlead.New / NewFromPurchaseSnapshot / calllog.New all call
// validateUUIDString) — surfaces if validation is bypassed.
var ErrInvalidUUID = invalidUUIDErr("crm integrationevents: invalid uuid")

type invalidUUIDErr string

func (u invalidUUIDErr) Error() string { return string(u) }
