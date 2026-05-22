package permissionrequest

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the SEALED marker interface for Request domain events.
// Sealed via the unexported isPermissionRequestEvent() method so only
// types in this package can satisfy it — same shape as role.Event +
// membership.Event.
//
// Per Vernon IDDD ch. 8 — domain events deliberately do NOT carry wire
// concerns (Topic / V1 alias). Wire-versioning lives in
// internal/identity/integrationevents/permission_request.go; the
// mapper there type-switches on these structs and emits the canonical
// V1 envelope.
type Event interface {
	isPermissionRequestEvent()
}

// RequestedEvent fires when a brand-new Pending Request is constructed
// via [New]. Carries the full identity tuple so subscribers (audit log,
// future SMS / push approval notifications) don't need to re-load the
// aggregate.
type RequestedEvent struct {
	RequestID             ID
	TenantID              tenant.ID
	RequesterMembershipID membership.ID
	Permission            string // wire-stable constant (e.g. "identity.users.create")
	DurationDays          int
	Reason                string
	At                    time.Time
}

func (RequestedEvent) isPermissionRequestEvent() {}

// ApprovedEvent fires when an approver successfully transitions a
// Pending request to Approved. ExpiresAt mirrors the bounded grant
// timestamp written to the requester's membership overlay.
type ApprovedEvent struct {
	RequestID            ID
	TenantID             tenant.ID
	ApproverMembershipID membership.ID
	ExpiresAt            time.Time
	At                   time.Time
}

func (ApprovedEvent) isPermissionRequestEvent() {}

// DeniedEvent fires when an approver rejects a Pending request.
// Reason is REQUIRED on Deny per ADR 0055 (audit canon — denials must
// carry context so the requester + audit trail know WHY).
type DeniedEvent struct {
	RequestID            ID
	TenantID             tenant.ID
	ApproverMembershipID membership.ID
	Reason               string
	At                   time.Time
}

func (DeniedEvent) isPermissionRequestEvent() {}

// CancelledEvent fires when the requester cancels their own Pending
// request. No approver involved (the requester is the sole caller).
type CancelledEvent struct {
	RequestID ID
	TenantID  tenant.ID
	At        time.Time
}

func (CancelledEvent) isPermissionRequestEvent() {}
