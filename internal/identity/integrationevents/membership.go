package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// MembershipCreatedV1 — a Person joined a tenant (or rejoined after a
// gap). Consumed by Tasks/CRM/etc. for default permission-set seeding
// + future hierarchy initialisation.
type MembershipCreatedV1 struct {
	MembershipID    uuid.UUID `json:"membership_id"`
	PersonID        uuid.UUID `json:"person_id"`
	TenantIDClaim   uuid.UUID `json:"tenant_id"`
	OccurredAtUTC   time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (MembershipCreatedV1) Topic() string { return "identity.membership_created.v1" }

// OccurredAt returns the domain timestamp.
func (e MembershipCreatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped]. Method form (not direct field
// read) keeps the marker interface consistent across types — some
// future events may compute the tenant from a richer field set.
func (e MembershipCreatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// MembershipDeactivatedV1 — Membership marked Inactive (job change,
// admin deactivation). Consumed by CRM (reassign open leads), Tasks
// (reassign open work items), Notifications (silence per-user channels).
type MembershipDeactivatedV1 struct {
	MembershipID  uuid.UUID `json:"membership_id"`
	PersonID      uuid.UUID `json:"person_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	Reason        string    `json:"reason"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (MembershipDeactivatedV1) Topic() string { return "identity.membership_deactivated.v1" }

// OccurredAt returns the domain timestamp.
func (e MembershipDeactivatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e MembershipDeactivatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// MembershipReactivatedV1 — Inactive → Active (e.g. re-hire). Consumers
// re-enable per-user channels closed by [MembershipDeactivatedV1].
type MembershipReactivatedV1 struct {
	MembershipID  uuid.UUID `json:"membership_id"`
	PersonID      uuid.UUID `json:"person_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (MembershipReactivatedV1) Topic() string { return "identity.membership_reactivated.v1" }

// OccurredAt returns the domain timestamp.
func (e MembershipReactivatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e MembershipReactivatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Compile-time assertions + registration.
var (
	_ TenantScoped = MembershipCreatedV1{}
	_ TenantScoped = MembershipDeactivatedV1{}
	_ TenantScoped = MembershipReactivatedV1{}

	_ = register(MembershipCreatedV1{})
	_ = register(MembershipDeactivatedV1{})
	_ = register(MembershipReactivatedV1{})
)
