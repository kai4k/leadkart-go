package integrationevents

import (
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
)

// PolicyChangedV1 — a tenant's commerce policy is at Version. Carries the WHOLE
// policy, never a delta. Wire alias: `companysettings.policy_changed.v1`.
// Tenant-scoped.
type PolicyChangedV1 struct {
	TenantIDClaim uuid.UUID             `json:"tenant_id"`
	Version       int64                 `json:"version"`
	Policy        commercepolicy.Policy `json:"policy"`
	ChangedKeys   []string              `json:"changed_keys,omitzero"`
	Reason        string                `json:"reason,omitzero"`
	// ChangedByMembershipID is empty for the system (the seeded version).
	ChangedByMembershipID string    `json:"changed_by_membership_id,omitzero"`
	OccurredAtUTC         time.Time `json:"occurred_at_utc"`
}

// PolicyRepublishedV1 — the daily anti-entropy job re-announced a tenant's
// CURRENT stored version; nothing changed. Wire alias:
// `companysettings.policy_republished.v1`. It is its own alias so it is not
// mistaken for a change: the replicas apply it with the same code, and it stays
// out of the tenant's audit timeline. Tenant-scoped.
type PolicyRepublishedV1 struct {
	TenantIDClaim uuid.UUID             `json:"tenant_id"`
	Version       int64                 `json:"version"`
	Policy        commercepolicy.Policy `json:"policy"`
	OccurredAtUTC time.Time             `json:"occurred_at_utc"`
}

var (
	_              = register(PolicyChangedV1{})
	_              = register(PolicyRepublishedV1{})
	_ TenantScoped = PolicyChangedV1{}
	_ TenantScoped = PolicyRepublishedV1{}
)

// Topic returns the canonical wire alias.
func (PolicyChangedV1) Topic() string { return "companysettings.policy_changed.v1" }

// OccurredAt returns the domain timestamp.
func (e PolicyChangedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e PolicyChangedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Topic returns the canonical wire alias.
func (PolicyRepublishedV1) Topic() string { return "companysettings.policy_republished.v1" }

// OccurredAt returns the domain timestamp.
func (e PolicyRepublishedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e PolicyRepublishedV1) TenantID() uuid.UUID { return e.TenantIDClaim }
