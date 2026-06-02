package quotation

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the sealed marker every domain event satisfies.
type Event interface{ isQuotationEvent() }

// CreatedEvent fires on ctor. Carries row IDs + line-item count only;
// items ride the wire integration event. Counts are int64 to sidestep
// G115 narrowing on platform-int len().
type CreatedEvent struct {
	QuotationID           ID
	TenantID              tenant.ID
	CustomerLeadID        CustomerLeadID
	LineItemCount         int64
	CreatedAt             time.Time
	CreatedByMembershipID membership.ID
}

func (CreatedEvent) isQuotationEvent() {}

// RevisedEvent fires on every Revise mutator call.
type RevisedEvent struct {
	QuotationID         ID
	TenantID            tenant.ID
	RevisionNumber      int64
	LineItemCount       int64
	Note                string
	RevisedAt           time.Time
	RevisedByMembership membership.ID
}

func (RevisedEvent) isQuotationEvent() {}

// ApprovedEvent fires on Approve. Carries the frozen revision number so
// the Order side knows which items snapshot was locked in.
type ApprovedEvent struct {
	QuotationID            ID
	TenantID               tenant.ID
	CustomerLeadID         CustomerLeadID
	ApprovedRevisionNumber int64
	ApprovedAt             time.Time
	ApprovedByMembership   membership.ID
}

func (ApprovedEvent) isQuotationEvent() {}

// RejectedEvent fires on Reject. Reason is operator-supplied.
type RejectedEvent struct {
	QuotationID          ID
	TenantID             tenant.ID
	Reason               string
	RejectedAt           time.Time
	RejectedByMembership membership.ID
}

func (RejectedEvent) isQuotationEvent() {}
