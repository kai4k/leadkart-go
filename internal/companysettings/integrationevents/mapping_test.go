package integrationevents_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
	"github.com/leadkart/leadkart-go/internal/companysettings/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func TestFromDomainEvent_PolicyChangedCarriesTheWholePolicy(t *testing.T) {
	t.Parallel()
	tid := uuid.New()
	actor := uuid.New()
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	policy := commercepolicy.Default()
	policy.Expiry.StopMonthsBefore = 3
	got, err := integrationevents.FromDomainEvent(commercesettings.PolicyChangedEvent{
		TenantID: tenant.ID(tid.String()), Version: 4, Policy: policy,
		ChangedKeys: []string{"expiry.stop_months_before"}, Reason: "audit",
		Actor: commercesettings.Actor{MembershipID: membership.ID(actor.String())}, At: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := integrationevents.PolicyChangedV1{
		TenantIDClaim: tid, Version: 4, Policy: policy,
		ChangedKeys: []string{"expiry.stop_months_before"}, Reason: "audit",
		ChangedByMembershipID: actor.String(), OccurredAtUTC: at,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatal(diff)
	}
	if got.Topic() != "companysettings.policy_changed.v1" {
		t.Fatalf("alias %q", got.Topic())
	}
}

func TestFromDomainEvent_RepublishHasItsOwnAliasSoItIsNotMistakenForAChange(t *testing.T) {
	t.Parallel()
	tid := uuid.New()
	got, err := integrationevents.FromDomainEvent(commercesettings.PolicyRepublishedEvent{
		TenantID: tenant.ID(tid.String()), Version: 2, Policy: commercepolicy.Default(), At: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := got.(integrationevents.PolicyRepublishedV1)
	if !ok || ev.Version != 2 || ev.Topic() != "companysettings.policy_republished.v1" {
		t.Fatalf("event = %+v", ev)
	}
}
