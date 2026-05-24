package integrationevents_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
)

// fixedTime returns a deterministic UTC instant for round-trip
// equality assertions. Production maps `fixedNow.UTC()`; tests
// pin a known value so any post-mapping mutation surfaces.
func fixedTime() time.Time {
	return time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
}

func TestFromDomainEvent_TenantRegistered_RoundTrip(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(ids.NewV7().String())
	s, _ := slug.New("acme-pharma")
	addr, _ := email.New("admin@acme.test")
	at := fixedTime()

	got, err := integrationevents.FromDomainEvent(tenant.RegisteredEvent{
		TenantID:    tid,
		Slug:        s,
		LegalName:   "Acme Pharma Pvt Ltd",
		DisplayName: "Acme",
		AdminEmail:  addr,
		At:          at,
	})
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	v1, ok := got.(integrationevents.TenantRegisteredV1)
	if !ok {
		t.Fatalf("type: got %T", got)
	}
	if v1.Slug != "acme-pharma" {
		t.Fatalf("slug: %q", v1.Slug)
	}
	if v1.AdminEmail != "admin@acme.test" {
		t.Fatalf("email: %q", v1.AdminEmail)
	}
	if !v1.OccurredAtUTC.Equal(at) {
		t.Fatalf("time: %v", v1.OccurredAtUTC)
	}
	if v1.Topic() != "identity.tenant_registered.v1" {
		t.Fatalf("topic: %q", v1.Topic())
	}
}

func TestFromDomainEvent_PersonAnonymised_PlatformScoped(t *testing.T) {
	t.Parallel()
	pid := person.ID(ids.NewV7().String())
	got, err := integrationevents.FromDomainEvent(person.AnonymisedEvent{
		PersonID: pid,
		At:       fixedTime(),
	})
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	v1, ok := got.(integrationevents.PersonAnonymisedV1)
	if !ok {
		t.Fatalf("type: got %T", got)
	}
	// Compile-time guarantees Platform; runtime confirm.
	if _, isPlatform := any(v1).(integrationevents.Platform); !isPlatform {
		t.Fatal("PersonAnonymisedV1 must satisfy Platform marker")
	}
	if v1.Topic() != "identity.person_anonymised.v1" {
		t.Fatalf("topic: %q", v1.Topic())
	}
}

func TestFromDomainEvent_MembershipCreated_TenantScoped(t *testing.T) {
	t.Parallel()
	mid := membership.ID(ids.NewV7().String())
	pid := person.ID(ids.NewV7().String())
	tid := tenant.ID(ids.NewV7().String())
	got, err := integrationevents.FromDomainEvent(membership.CreatedEvent{
		MembershipID: mid,
		PersonID:     pid,
		TenantID:     tid,
		At:           fixedTime(),
	})
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	v1, ok := got.(integrationevents.MembershipCreatedV1)
	if !ok {
		t.Fatalf("type: got %T", got)
	}
	if v1.TenantID().String() != tid.String() {
		t.Fatalf("tenant_id: %s want %s", v1.TenantID(), tid)
	}
	// Compile-time guarantees TenantScoped; runtime confirm.
	if _, isTS := any(v1).(integrationevents.TenantScoped); !isTS {
		t.Fatal("MembershipCreatedV1 must satisfy TenantScoped marker")
	}
}

func TestFromDomainEvent_RefreshTokenRotated_CarriesPersonAndTenant(t *testing.T) {
	t.Parallel()
	fid := refreshtoken.FamilyID(ids.NewV7().String())
	pid := person.ID(ids.NewV7().String())
	tid := tenant.ID(ids.NewV7().String())
	consumedID := refreshtoken.TokenID(ids.NewV7().String())
	newID := refreshtoken.TokenID(ids.NewV7().String())

	got, err := integrationevents.FromDomainEvent(refreshtoken.RotatedEvent{
		FamilyID:           fid,
		PersonID:           pid,
		TenantID:           tid,
		ConsumedTokenID:    consumedID,
		NewTokenID:         newID,
		NewTokenGeneration: 1,
		At:                 fixedTime(),
	})
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	v1, ok := got.(integrationevents.RefreshTokenRotatedV1)
	if !ok {
		t.Fatalf("type: got %T", got)
	}
	if v1.PersonID.String() != pid.String() {
		t.Fatalf("person_id: %s", v1.PersonID)
	}
	if v1.TenantID().String() != tid.String() {
		t.Fatalf("tenant_id: %s", v1.TenantID())
	}
	if v1.NewTokenGeneration != 1 {
		t.Fatalf("generation: %d", v1.NewTokenGeneration)
	}
}

func TestFromDomainEvent_RefreshTokenRevoked_ReuseDetected(t *testing.T) {
	t.Parallel()
	got, err := integrationevents.FromDomainEvent(refreshtoken.RevokedEvent{
		FamilyID: refreshtoken.FamilyID(ids.NewV7().String()),
		PersonID: person.ID(ids.NewV7().String()),
		TenantID: tenant.ID(ids.NewV7().String()),
		Reason:   "reuse_detected",
		At:       fixedTime(),
	})
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	v1, ok := got.(integrationevents.RefreshTokenFamilyRevokedV1)
	if !ok {
		t.Fatalf("type: got %T", got)
	}
	if v1.Reason != "reuse_detected" {
		t.Fatalf("reason: %q", v1.Reason)
	}
}

func TestFromDomainEvent_UnknownType_ReturnsErrUnknown(t *testing.T) {
	t.Parallel()
	type bogusEvent struct{ X int }
	_, err := integrationevents.FromDomainEvent(bogusEvent{X: 1})
	if !errors.Is(err, integrationevents.ErrUnknownDomainEvent) {
		t.Fatalf("expected ErrUnknownDomainEvent, got %v", err)
	}
}
