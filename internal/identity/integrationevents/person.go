package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// PersonCreatedV1 — a new global Person identity has been added (first
// time this email enters the system). Consumed by Notifications
// (welcome email) + future modules.
//
// Platform-scoped: Person is global identity; no per-tenant context
// per `multi-tenancy.md` "Identity model".
type PersonCreatedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	Email         string    `json:"email"`
	FirstName     string    `json:"first_name"`
	LastName      string    `json:"last_name"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonCreatedV1) Topic() string { return "identity.person_created.v1" }

// OccurredAt returns the domain timestamp.
func (e PersonCreatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonPasswordChangedV1 — Person rotated their password. The
// SecurityStamp on the Person changed; Identity subscribers MUST
// revoke every refresh-token family for this Person across tenants
// (logout-all-sessions choreography per `security.md` "SecurityStamp
// rotation triggers").
type PersonPasswordChangedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonPasswordChangedV1) Topic() string { return "identity.person_password_changed.v1" }

// OccurredAt returns the domain timestamp.
func (e PersonPasswordChangedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonProfileUpdatedV1 — Person changed their display name
// (FirstName + LastName). Platform-scoped per the .NET parent's
// vocabulary split (Person fields global; Membership profile fields
// tenant-scoped — see MembershipProfileUpdatedV1 for the tenant-scoped
// counterpart). Consumed by Notifications (display-name update) +
// audit.
type PersonProfileUpdatedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	OldFirstName  string    `json:"old_first_name"`
	OldLastName   string    `json:"old_last_name"`
	NewFirstName  string    `json:"new_first_name"`
	NewLastName   string    `json:"new_last_name"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonProfileUpdatedV1) Topic() string { return "identity.person_profile_updated.v1" }

// OccurredAt returns the domain timestamp.
func (e PersonProfileUpdatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonGloballySuspendedV1 — Person was globally banned (compliance,
// fraud, cross-tenant abuse). Distinct from PersonAnonymisedV1
// (irreversible PII scrub) and Membership.Deactivated (per-tenant).
//
// Auth subscribers MUST kill every refresh-token family for this
// PersonID across tenants AND block login attempts. Notifications +
// SIEM subscribers may surface alerts.
type PersonGloballySuspendedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	Reason        string    `json:"reason"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonGloballySuspendedV1) Topic() string { return "identity.person_globally_suspended.v1" }

// OccurredAt returns the domain timestamp.
func (e PersonGloballySuspendedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonGlobalSuspensionLiftedV1 — global suspension reversed by
// operator. Subscribers re-enable login + remove SIEM block.
type PersonGlobalSuspensionLiftedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonGlobalSuspensionLiftedV1) Topic() string {
	return "identity.person_global_suspension_lifted.v1"
}

// OccurredAt returns the domain timestamp.
func (e PersonGlobalSuspensionLiftedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// PersonAnonymisedV1 — DPDP Act §12 / GDPR Art. 17 right-to-erasure
// completed at the Person aggregate. Cascades to every module touching
// the Person's PII per `data-retention.md` (CRM lead notes scrub,
// Tasks comment scrub, etc.) + Identity revokes every refresh-token
// family across tenants.
type PersonAnonymisedV1 struct {
	platformMarker

	PersonID      uuid.UUID `json:"person_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (PersonAnonymisedV1) Topic() string { return "identity.person_anonymised.v1" }

// OccurredAt returns the domain timestamp.
func (e PersonAnonymisedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// Compile-time assertions + registration.
var (
	_ Platform = PersonCreatedV1{}
	_ Platform = PersonPasswordChangedV1{}
	_ Platform = PersonProfileUpdatedV1{}
	_ Platform = PersonGloballySuspendedV1{}
	_ Platform = PersonGlobalSuspensionLiftedV1{}
	_ Platform = PersonAnonymisedV1{}

	_ = register(PersonCreatedV1{})
	_ = register(PersonPasswordChangedV1{})
	_ = register(PersonProfileUpdatedV1{})
	_ = register(PersonGloballySuspendedV1{})
	_ = register(PersonGlobalSuspensionLiftedV1{})
	_ = register(PersonAnonymisedV1{})
)
