// Package assignmenthistory defines the AssignmentHistory aggregate per
// ADR 0060.
//
// AssignmentHistory is the append-only assignment audit for a CrmLead.
// Each row captures one assignment event (who was assigned, by whom,
// when, optional reason).
//
// Per ADR 0060: the latest row by [Entry.AssignedAt] for a given lead IS
// the current assignee — but for hot-path reads the value is mirrored
// onto [crmlead.CrmLead.AssigneeMembershipID]. The history table is the
// audit/forensic-trail source of truth.
//
// Slice 1 emits the audit row + integration event from the same command
// handler as [crmlead.Assign]; query endpoints for the full history are
// deferred to Slice 2.
package assignmenthistory

import (
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrInvalid is the sentinel returned (wrapped via %w) by [New] on
// invariant violation.
var ErrInvalid = errs.New(errs.KindInvalidInput, "assignment_history", "invalid assignment history entry")

// ID is the entry primary key — UUIDv7 string for B-tree locality.
type ID string

// IsZero reports whether ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

const reasonMaxLen = 1000

// Entry is the aggregate root. Append-only.
//
// Invariants enforced by [New]:
//   - ID + TenantID + LeadID non-zero.
//   - AssigneeMembershipID + AssignedByMembershipID non-empty.
//   - Reason ≤ reasonMaxLen.
//   - AssignedAt non-zero.
//
// PreviousAssignee is empty on the FIRST assignment for a lead.
type Entry struct {
	id                       ID
	tenantID                 tenant.ID
	leadID                   crmlead.ID
	previousAssignee         string // empty for first assignment
	assigneeMembershipID     string
	assignedByMembershipID   string
	reason                   string
	assignedAt               time.Time
	createdAt                time.Time
}

// New constructs a brand-new audit Entry. Append-only — no
// state-mutation methods after construction.
//
// Domain-event emission is NOT done here — assignment audit is
// orchestrated by the same command handler that mutates the CrmLead;
// the lead aggregate emits [crmlead.AssignedEvent] and the history
// row is a side-effect write. Keeping the entry mute keeps the
// integration-event vocabulary single-sourced (lead-assigned, not
// assignment-recorded).
//
// `now` is the injected clock (Pure Domain canon — ADR 0047). Caller
// (handler) passes the same wall-clock used for the lead-aggregate
// mutator so the audit row's CreatedAt aligns with AssignedAt.
func New(id ID, tenantID tenant.ID, leadID crmlead.ID, previousAssignee, assignee, assignedBy, reason string, assignedAt time.Time, now time.Time) (*Entry, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if strings.TrimSpace(tenantID.String()) == "" {
		return nil, fmt.Errorf("%w: tenant id required", ErrInvalid)
	}
	if leadID.IsZero() {
		return nil, fmt.Errorf("%w: lead id required", ErrInvalid)
	}
	if strings.TrimSpace(assignee) == "" {
		return nil, fmt.Errorf("%w: assignee membership id required", ErrInvalid)
	}
	if strings.TrimSpace(assignedBy) == "" {
		return nil, fmt.Errorf("%w: assigned-by membership id required", ErrInvalid)
	}
	if len(reason) > reasonMaxLen {
		return nil, fmt.Errorf("%w: reason too long (max %d, got %d)", ErrInvalid, reasonMaxLen, len(reason))
	}
	if assignedAt.IsZero() {
		return nil, fmt.Errorf("%w: assigned_at required", ErrInvalid)
	}
	if now.IsZero() {
		return nil, fmt.Errorf("%w: now required", ErrInvalid)
	}
	return &Entry{
		id:                     id,
		tenantID:               tenantID,
		leadID:                 leadID,
		previousAssignee:       previousAssignee,
		assigneeMembershipID:   assignee,
		assignedByMembershipID: assignedBy,
		reason:                 reason,
		assignedAt:             assignedAt,
		createdAt:              now,
	}, nil
}

// Snapshot is the persistence-layer DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID                     ID
	TenantID               tenant.ID
	LeadID                 crmlead.ID
	PreviousAssignee       string
	AssigneeMembershipID   string
	AssignedByMembershipID string
	Reason                 string
	AssignedAt             time.Time
	CreatedAt              time.Time
}

// UnmarshalFromDB re-hydrates an Entry from persistence.
func UnmarshalFromDB(s Snapshot) *Entry {
	return &Entry{
		id:                     s.ID,
		tenantID:               s.TenantID,
		leadID:                 s.LeadID,
		previousAssignee:       s.PreviousAssignee,
		assigneeMembershipID:   s.AssigneeMembershipID,
		assignedByMembershipID: s.AssignedByMembershipID,
		reason:                 s.Reason,
		assignedAt:             s.AssignedAt,
		createdAt:              s.CreatedAt,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the entry primary key.
func (e *Entry) ID() ID { return e.id }

// TenantID returns the owning tenant.
func (e *Entry) TenantID() tenant.ID { return e.tenantID }

// LeadID returns the parent lead.
func (e *Entry) LeadID() crmlead.ID { return e.leadID }

// PreviousAssignee returns the assignee being replaced, empty on the
// first assignment for the lead.
func (e *Entry) PreviousAssignee() string { return e.previousAssignee }

// AssigneeMembershipID returns the newly-assigned member.
func (e *Entry) AssigneeMembershipID() string { return e.assigneeMembershipID }

// AssignedByMembershipID returns the actor who performed the assignment.
func (e *Entry) AssignedByMembershipID() string { return e.assignedByMembershipID }

// Reason returns the optional audit reason supplied at assignment.
func (e *Entry) Reason() string { return e.reason }

// AssignedAt returns the wall-clock time of the assignment.
func (e *Entry) AssignedAt() time.Time { return e.assignedAt }

// CreatedAt returns the row-insert timestamp.
func (e *Entry) CreatedAt() time.Time { return e.createdAt }
