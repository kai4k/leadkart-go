package integrationevents

import (
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
)

// FromDomainEvent translates ANY recognised Tasks domain event into
// its canonical integration event. Used by the repository adapters'
// drain helpers: domain events emitted by aggregates flow through
// this function before they are written to the shared common.outbox relay.
//
// Panics on an UNKNOWN domain event type (default branch) per the
// established CRM canon (reviewer H5) — programmer error, fail-loud
// at the boundary so the bug surfaces in tests + dev rather than as
// a silent drop in the request path.
//
//nolint:cyclop // Switch dispatcher — one case per recognised event.
func FromDomainEvent(d any) (Event, error) {
	switch e := d.(type) {

	case workitem.CreatedEvent:
		workItemID, err := parseUUID("work_item_id", e.WorkItemID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		assignedTo, err := parseUUID("assigned_to_membership_id", e.AssignedToMembershipID)
		if err != nil {
			return nil, err
		}
		assignedBy, err := parseUUID("assigned_by_membership_id", e.AssignedByMembershipID)
		if err != nil {
			return nil, err
		}
		createdBy, err := parseUUID("created_by_membership_id", e.CreatedByMembershipID)
		if err != nil {
			return nil, err
		}
		var batchID uuid.UUID
		if e.BatchID != "" {
			batchID, err = parseUUID("batch_id", e.BatchID)
			if err != nil {
				return nil, err
			}
		}
		return WorkItemCreatedV1{
			WorkItemID:             workItemID,
			TenantIDClaim:          tenantID,
			Type:                   e.Type.String(),
			Priority:               e.Priority.String(),
			Title:                  e.Title,
			AssignedToMembershipID: assignedTo,
			AssignedByMembershipID: assignedBy,
			DueAtUTC:               e.DueAt.UTC(),
			BatchID:                batchID,
			SourceModule:           e.SourceModule,
			SourceEntityType:       e.SourceEntityType,
			SourceEntityID:         e.SourceEntityID,
			CreatedByMembershipID:  createdBy,
			OccurredAtUTC:          e.At.UTC(),
		}, nil

	case workitem.StartedEvent:
		// Started is intentionally NOT a wire-published integration event
		// — no downstream consumer in v0.2. Returning a nil-event +
		// nil-error here would defeat the H5 fail-loud invariant, so we
		// emit a dedicated transitional event-mapper exclusion. The
		// outbox writer in adapters/outbox_writer.go filters Started
		// before calling FromDomainEvent.
		return nil, errInternalNotMapped("StartedEvent")

	case workitem.CompletedEvent:
		workItemID, err := parseUUID("work_item_id", e.WorkItemID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		actor, err := parseUUID("actor_membership_id", e.ActorID)
		if err != nil {
			return nil, err
		}
		return WorkItemCompletedV1{
			WorkItemID:    workItemID,
			TenantIDClaim: tenantID,
			ActorID:       actor,
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case workitem.CancelledEvent:
		workItemID, err := parseUUID("work_item_id", e.WorkItemID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		actor, err := parseUUID("actor_membership_id", e.ActorID)
		if err != nil {
			return nil, err
		}
		return WorkItemCancelledV1{
			WorkItemID:    workItemID,
			TenantIDClaim: tenantID,
			ActorID:       actor,
			Reason:        e.Reason,
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case workitem.OverdueEvent:
		workItemID, err := parseUUID("work_item_id", e.WorkItemID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		assignedTo, err := parseUUID("assigned_to_membership_id", e.AssignedToMembershipID)
		if err != nil {
			return nil, err
		}
		return WorkItemOverdueV1{
			WorkItemID:             workItemID,
			TenantIDClaim:          tenantID,
			AssignedToMembershipID: assignedTo,
			DueAtUTC:               e.DueAt.UTC(),
			OccurredAtUTC:          e.At.UTC(),
		}, nil

	case workitem.ReassignedEvent:
		workItemID, err := parseUUID("work_item_id", e.WorkItemID.String())
		if err != nil {
			return nil, err
		}
		tenantID, err := parseUUID("tenant_id", e.TenantID.String())
		if err != nil {
			return nil, err
		}
		newAssignee, err := parseUUID("new_assignee_membership_id", e.NewAssigneeMembershipID)
		if err != nil {
			return nil, err
		}
		reassignedBy, err := parseUUID("reassigned_by_membership_id", e.ReassignedByMembershipID)
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
		return WorkItemAssignedV1{
			WorkItemID:               workItemID,
			TenantIDClaim:            tenantID,
			PreviousAssignee:         prev,
			NewAssigneeMembershipID:  newAssignee,
			ReassignedByMembershipID: reassignedBy,
			Reason:                   e.Reason,
			OccurredAtUTC:            e.At.UTC(),
		}, nil

	default:
		panic(fmt.Sprintf("tasks integrationevents: unmapped domain event %T", d))
	}
}

// parseUUID is the error-returning UUID parser used by FromDomainEvent.
func parseUUID(name, s string) (uuid.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %s=%q: %w", ErrInvalidUUID, name, s, err)
	}
	return u, nil
}

// ErrInvalidUUID is the sentinel returned by [parseUUID] / FromDomainEvent
// when a domain ID string fails to parse.
var ErrInvalidUUID = invalidUUIDErr("tasks integrationevents: invalid uuid")

type invalidUUIDErr string

func (u invalidUUIDErr) Error() string { return string(u) }

// errInternalNotMapped is the sentinel returned from FromDomainEvent
// for domain events that the adapter is expected to FILTER BEFORE
// calling FromDomainEvent (e.g. StartedEvent — no wire-published
// counterpart in v0.2). The adapter checks IsNotMapped(err) and skips
// the event on a true.
func errInternalNotMapped(name string) error {
	return notMappedErr(name)
}

type notMappedErr string

func (n notMappedErr) Error() string {
	return fmt.Sprintf("tasks integrationevents: %s has no wire counterpart", string(n))
}

// IsNotMapped reports whether err is the not-mapped sentinel. Adapter
// drain helpers check this to skip filtered events.
func IsNotMapped(err error) bool {
	var n notMappedErr
	return errors.As(err, &n)
}
