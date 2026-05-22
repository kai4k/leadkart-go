package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// PermissionRequestSubmittedV1 — a new Pending permission-elevation
// request was submitted per ADR 0055. Subscribers: audit log; future
// SMS / push approval notifications to the approver (deferred per
// ADR 0055 deferred-work list).
//
// Tenant-scoped: requests are per-tenant and RLS bound.
type PermissionRequestSubmittedV1 struct {
	RequestID             uuid.UUID `json:"request_id"`
	TenantIDClaim         uuid.UUID `json:"tenant_id"`
	RequesterMembershipID uuid.UUID `json:"requester_membership_id"`
	Permission            string    `json:"permission"`
	DurationDays          int       `json:"duration_days"`
	Reason                string    `json:"reason"`
	OccurredAtUTC         time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PermissionRequestSubmittedV1) Topic() string {
	return "identity.permission_request_submitted.v1"
}

// OccurredAt returns the domain timestamp.
func (e PermissionRequestSubmittedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e PermissionRequestSubmittedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// PermissionRequestApprovedV1 — a request was approved + a time-bound
// grant landed on the requester's membership overlay. ExpiresAtUTC
// mirrors the grant's expiry.
type PermissionRequestApprovedV1 struct {
	RequestID            uuid.UUID `json:"request_id"`
	TenantIDClaim        uuid.UUID `json:"tenant_id"`
	ApproverMembershipID uuid.UUID `json:"approver_membership_id"`
	ExpiresAtUTC         time.Time `json:"expires_at_utc"`
	OccurredAtUTC        time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PermissionRequestApprovedV1) Topic() string { return "identity.permission_request_approved.v1" }

// OccurredAt returns the domain timestamp.
func (e PermissionRequestApprovedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e PermissionRequestApprovedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// PermissionRequestDeniedV1 — a request was rejected. Reason is
// REQUIRED on Deny per ADR 0055 audit canon.
type PermissionRequestDeniedV1 struct {
	RequestID            uuid.UUID `json:"request_id"`
	TenantIDClaim        uuid.UUID `json:"tenant_id"`
	ApproverMembershipID uuid.UUID `json:"approver_membership_id"`
	Reason               string    `json:"reason"`
	OccurredAtUTC        time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PermissionRequestDeniedV1) Topic() string { return "identity.permission_request_denied.v1" }

// OccurredAt returns the domain timestamp.
func (e PermissionRequestDeniedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e PermissionRequestDeniedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// PermissionRequestCancelledV1 — the requester withdrew their pending
// request. No approver involved.
type PermissionRequestCancelledV1 struct {
	RequestID     uuid.UUID `json:"request_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PermissionRequestCancelledV1) Topic() string {
	return "identity.permission_request_cancelled.v1"
}

// OccurredAt returns the domain timestamp.
func (e PermissionRequestCancelledV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e PermissionRequestCancelledV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Compile-time + runtime registration.
var (
	_ TenantScoped = PermissionRequestSubmittedV1{}
	_ TenantScoped = PermissionRequestApprovedV1{}
	_ TenantScoped = PermissionRequestDeniedV1{}
	_ TenantScoped = PermissionRequestCancelledV1{}

	_ = register(PermissionRequestSubmittedV1{})
	_ = register(PermissionRequestApprovedV1{})
	_ = register(PermissionRequestDeniedV1{})
	_ = register(PermissionRequestCancelledV1{})
)
