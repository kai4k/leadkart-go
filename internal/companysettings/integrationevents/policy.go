package integrationevents

import (
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
)

// PolicyChangedV1 — a tenant's commerce policy is at Version. Carries the WHOLE
// policy, never a delta. Wire alias: `companysettings.policy_changed.v1`.
// Tenant-scoped.
//
// Republish is true when the daily anti-entropy job re-announced the current
// version rather than a change being made.
type PolicyChangedV1 struct {
	TenantIDClaim uuid.UUID             `json:"tenant_id"`
	Version       int64                 `json:"version"`
	Policy        commercepolicy.Policy `json:"policy"`
	ChangedKeys   []string              `json:"changed_keys,omitzero"`
	Reason        string                `json:"reason,omitzero"`
	// ChangedByMembershipID is empty for the system (the seeded version).
	ChangedByMembershipID string    `json:"changed_by_membership_id,omitzero"`
	Republish             bool      `json:"republish,omitzero"`
	OccurredAtUTC         time.Time `json:"occurred_at_utc"`
}

var (
	_              = register(PolicyChangedV1{})
	_ TenantScoped = PolicyChangedV1{}
)

// Topic returns the canonical wire alias.
func (PolicyChangedV1) Topic() string { return "companysettings.policy_changed.v1" }

// OccurredAt returns the domain timestamp.
func (e PolicyChangedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e PolicyChangedV1) TenantID() uuid.UUID { return e.TenantIDClaim }
