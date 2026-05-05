package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// RefreshTokenFamilyCreatedV1 — login flow opened a fresh refresh-token
// family for a (Person, Tenant) session. Consumed by Notifications
// ("new device sign-in" alert) + audit log.
type RefreshTokenFamilyCreatedV1 struct {
	FamilyID      uuid.UUID `json:"family_id"`
	PersonID      uuid.UUID `json:"person_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	DeviceLabel   string    `json:"device_label"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (RefreshTokenFamilyCreatedV1) Topic() string {
	return "identity.refresh_token_family_created.v1"
}

// OccurredAt returns the domain timestamp.
func (e RefreshTokenFamilyCreatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e RefreshTokenFamilyCreatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// RefreshTokenRotatedV1 — successful rotation; previous-generation
// token consumed, new generation issued. Mostly an audit signal;
// hot path so consumers stay light.
type RefreshTokenRotatedV1 struct {
	FamilyID            uuid.UUID `json:"family_id"`
	PersonID            uuid.UUID `json:"person_id"`
	TenantIDClaim       uuid.UUID `json:"tenant_id"`
	ConsumedTokenID     uuid.UUID `json:"consumed_token_id"`
	NewTokenID          uuid.UUID `json:"new_token_id"`
	NewTokenGeneration  int32     `json:"new_token_generation"`
	OccurredAtUTC       time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (RefreshTokenRotatedV1) Topic() string { return "identity.refresh_token_rotated.v1" }

// OccurredAt returns the domain timestamp.
func (e RefreshTokenRotatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e RefreshTokenRotatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// RefreshTokenFamilyRevokedV1 — family terminated. Reason field
// disambiguates the trigger:
//
//   - "user_logout" — user clicked logout.
//   - "admin_revoke" — operator revoked the session.
//   - "reuse_detected" — RFC 9700 §4.13 reuse-detection fired (security
//     incident; Notifications routes to SIEM + user alert).
//   - "password_changed" / "person_anonymised" — Identity-internal
//     cascade per the choreographed reactions.
//   - "family_cap_exceeded" — too many active families per Person;
//     oldest evicted.
//
// Consumers branch on Reason; "reuse_detected" is the load-bearing
// security path.
type RefreshTokenFamilyRevokedV1 struct {
	FamilyID      uuid.UUID `json:"family_id"`
	PersonID      uuid.UUID `json:"person_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	Reason        string    `json:"reason"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (RefreshTokenFamilyRevokedV1) Topic() string {
	return "identity.refresh_token_family_revoked.v1"
}

// OccurredAt returns the domain timestamp.
func (e RefreshTokenFamilyRevokedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e RefreshTokenFamilyRevokedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Compile-time assertions + registration.
var (
	_ TenantScoped = RefreshTokenFamilyCreatedV1{}
	_ TenantScoped = RefreshTokenRotatedV1{}
	_ TenantScoped = RefreshTokenFamilyRevokedV1{}

	_ = register(RefreshTokenFamilyCreatedV1{})
	_ = register(RefreshTokenRotatedV1{})
	_ = register(RefreshTokenFamilyRevokedV1{})
)
