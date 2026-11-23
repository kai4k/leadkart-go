package commercesettings

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the sealed marker every domain event satisfies.
type Event interface{ isCommerceSettingsEvent() }

// PolicyChangedEvent fires when the stored policy moves to a new version
// (including the first, seeded version). It carries the WHOLE policy at that
// version: a subscriber upserts it, never merges a delta, so a replayed or
// reordered event is harmless. The repository also writes it to
// companysettings.policy_history.
type PolicyChangedEvent struct {
	TenantID    tenant.ID
	Version     int64
	Policy      commercepolicy.Policy
	ChangedKeys []string
	Reason      string
	Actor       Actor
	At          time.Time
}

func (PolicyChangedEvent) isCommerceSettingsEvent() {}

// PolicyRepublishedEvent re-announces the CURRENT stored version without
// changing it (the daily anti-entropy job). It writes no history row.
type PolicyRepublishedEvent struct {
	TenantID tenant.ID
	Version  int64
	Policy   commercepolicy.Policy
	At       time.Time
}

func (PolicyRepublishedEvent) isCommerceSettingsEvent() {}
