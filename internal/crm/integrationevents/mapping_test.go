package integrationevents_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/integrationevents"
)

// All UUIDs used in this test are deterministic so a snapshot-shaped
// assertion is trivial to write.
var (
	leadID     = uuid.MustParse("01923400-0000-7000-8000-000000000001")
	tenantID   = uuid.MustParse("01923400-0000-7000-8000-000000000002")
	memberA    = uuid.MustParse("01923400-0000-7000-8000-000000000003")
	memberB    = uuid.MustParse("01923400-0000-7000-8000-000000000004")
	purchaseID = uuid.MustParse("01923400-0000-7000-8000-000000000005")
	callID     = uuid.MustParse("01923400-0000-7000-8000-000000000006")
	at         = time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
)

func TestFromDomainEvent_CreatedV1(t *testing.T) {
	t.Parallel()
	in := crmlead.CreatedEvent{
		LeadID: crmlead.ID(leadID.String()), TenantID: tenantID.String(),
		SourcePurchaseID: purchaseID.String(), CreatedByMembershipID: memberA.String(), At: at,
	}
	got, err := integrationevents.FromDomainEvent(in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v1, ok := got.(integrationevents.CrmLeadCreatedV1)
	if !ok {
		t.Fatalf("type: %T", got)
	}
	if v1.LeadID != leadID || v1.TenantIDClaim != tenantID ||
		v1.SourcePurchaseID != purchaseID || v1.CreatedByMembershipID != memberA {
		t.Fatalf("fields: %+v", v1)
	}
	if v1.Topic() != "crm.lead-created.v1" {
		t.Fatalf("topic: %q", v1.Topic())
	}
}

func TestFromDomainEvent_AssignedV1_FirstAndReassign(t *testing.T) {
	t.Parallel()
	first := crmlead.AssignedEvent{
		LeadID: crmlead.ID(leadID.String()), TenantID: tenantID.String(),
		AssigneeMembershipID: memberA.String(), AssignedByMembershipID: memberB.String(), At: at,
	}
	got, err := integrationevents.FromDomainEvent(first)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v1 := got.(integrationevents.CrmLeadAssignedV1)
	if v1.PreviousAssignee != uuid.Nil {
		t.Fatalf("first should be uuid.Nil prev, got %v", v1.PreviousAssignee)
	}
	if v1.AssigneeMembershipID != memberA {
		t.Fatalf("assignee: %v", v1.AssigneeMembershipID)
	}

	reassign := first
	reassign.PreviousAssignee = memberA.String()
	reassign.AssigneeMembershipID = memberB.String()
	got2, err := integrationevents.FromDomainEvent(reassign)
	if err != nil {
		t.Fatalf("reassign err: %v", err)
	}
	v2 := got2.(integrationevents.CrmLeadAssignedV1)
	if v2.PreviousAssignee != memberA {
		t.Fatalf("prev: %v", v2.PreviousAssignee)
	}
}

func TestFromDomainEvent_StageChangedAndTemperatureChanged(t *testing.T) {
	t.Parallel()
	sc := crmlead.StageChangedEvent{
		LeadID: crmlead.ID(leadID.String()), TenantID: tenantID.String(),
		OldStage: crmlead.StageNew, NewStage: crmlead.StageContacted,
		ChangedByMembershipID: memberA.String(), Reason: "first call", At: at,
	}
	got, err := integrationevents.FromDomainEvent(sc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v1 := got.(integrationevents.CrmLeadStageChangedV1)
	if v1.OldStage != "new" || v1.NewStage != "contacted" || v1.Reason != "first call" {
		t.Fatalf("stage v1: %+v", v1)
	}

	tc := crmlead.TemperatureChangedEvent{
		LeadID: crmlead.ID(leadID.String()), TenantID: tenantID.String(),
		OldTemperature: crmlead.TemperatureWarm, NewTemperature: crmlead.TemperatureHot,
		ChangedByMembershipID: memberA.String(), At: at,
	}
	got2, err := integrationevents.FromDomainEvent(tc)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v2 := got2.(integrationevents.CrmLeadTemperatureChangedV1)
	if v2.OldTemperature != "warm" || v2.NewTemperature != "hot" {
		t.Fatalf("temp v1: %+v", v2)
	}
}

func TestFromDomainEvent_ConvertedAndLost(t *testing.T) {
	t.Parallel()
	conv, err := integrationevents.FromDomainEvent(crmlead.ConvertedEvent{
		LeadID: crmlead.ID(leadID.String()), TenantID: tenantID.String(),
		ConvertedByMembershipID: memberA.String(), At: at,
	})
	if err != nil {
		t.Fatalf("conv: %v", err)
	}
	if conv.Topic() != "crm.lead-converted.v1" {
		t.Fatalf("topic: %q", conv.Topic())
	}

	lost, err := integrationevents.FromDomainEvent(crmlead.LostEvent{
		LeadID: crmlead.ID(leadID.String()), TenantID: tenantID.String(),
		LostByMembershipID: memberA.String(), Reason: "no budget", At: at,
	})
	if err != nil {
		t.Fatalf("lost: %v", err)
	}
	lv := lost.(integrationevents.CrmLeadLostV1)
	if lv.Reason != "no budget" {
		t.Fatalf("reason: %q", lv.Reason)
	}
}

func TestFromDomainEvent_CallLogged(t *testing.T) {
	t.Parallel()
	got, err := integrationevents.FromDomainEvent(calllog.LoggedEvent{
		CallID: calllog.ID(callID.String()), LeadID: crmlead.ID(leadID.String()), TenantID: tenantID.String(),
		Outcome: calllog.OutcomeInterested, LoggedByMembershipID: memberA.String(), At: at,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	v1 := got.(integrationevents.CrmCallLoggedV1)
	if v1.CallID != callID || v1.Outcome != "interested" {
		t.Fatalf("fields: %+v", v1)
	}
}

// TestFromDomainEvent_UnknownPanics pins reviewer H5: an unmapped
// domain event MUST fail-loud (panic), not silently return nil. The
// previous "(nil, ErrUnknownDomainEvent)" shape combined with the
// outbox writer's tolerant nil-skip path could silently drop new
// domain events whose integration counterpart was forgotten.
func TestFromDomainEvent_UnknownPanics(t *testing.T) {
	t.Parallel()
	type unknown struct{}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("FromDomainEvent(unknown): expected panic, got none")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic payload: %T %v", r, r)
		}
		if !strings.Contains(msg, "unmapped domain event") {
			t.Fatalf("panic msg: %q", msg)
		}
	}()
	_, _ = integrationevents.FromDomainEvent(unknown{})
}

// TestFromDomainEvent_MalformedUUIDReturnsErr pins reviewer H6: a
// malformed UUID in a domain event must surface as ErrInvalidUUID
// (not a panic) so the outbox writer can map it back to a clean
// error path. Normally impossible because aggregate constructors
// validate at construction time; this is defense-in-depth.
func TestFromDomainEvent_MalformedUUIDReturnsErr(t *testing.T) {
	t.Parallel()
	in := crmlead.CreatedEvent{
		LeadID:                crmlead.ID(leadID.String()),
		TenantID:              "not-a-uuid",
		CreatedByMembershipID: memberA.String(),
		At:                    at,
	}
	_, err := integrationevents.FromDomainEvent(in)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid uuid") {
		t.Fatalf("error: %v", err)
	}
}
