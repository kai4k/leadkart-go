package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/platform/app/command"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/integrationevents"
	"github.com/leadkart/leadkart-go/internal/platform/platformtest"
)

func sampleForm(t *testing.T) leadform.Form {
	t.Helper()
	f, err := leadform.New(leadform.Input{
		ContactName:    "Test Pharma",
		MobileE164:     "+919876543210",
		Pincode:        "411001",
		City:           "Pune",
		District:       "Pune",
		State:          "Maharashtra",
		BusinessType:   leadform.BusinessTypePCD,
		MedicineSystem: leadform.MedicineSystemAllopathic,
		OrderValue:     leadform.OrderValueUpto25000,
		BuyTimeline:    leadform.BuyTimelineWithin15Days,
	})
	if err != nil {
		t.Fatalf("sample form: %v", err)
	}
	return f
}

func nowFunc() time.Time {
	return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
}

func TestVerifyUnverifiedContact_HappyPath(t *testing.T) {
	t.Parallel()

	contacts := platformtest.NewFakeUnverifiedContactRepository()
	leads := platformtest.NewFakePlatformLeadRepository()
	outbox := platformtest.NewFakeOutbox()
	uow := platformtest.NewFakeUnitOfWork()

	// Seed: existing in-call contact.
	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	cID := unverifiedcontact.ID(ids.NewV7().String())
	c, err := unverifiedcontact.New(cID, sampleForm(t), agentID, nowFunc())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := c.StartCall(nowFunc()); err != nil {
		t.Fatalf("seed start call: %v", err)
	}
	if err := contacts.Add(t.Context(), c); err != nil {
		t.Fatalf("seed add: %v", err)
	}

	h := command.NewVerifyUnverifiedContactHandler(uow, contacts, leads, outbox, nowFunc, func() platformlead.ID { return platformlead.ID(ids.NewV7().String()) })
	out, err := h.Handle(t.Context(), command.VerifyUnverifiedContactCommand{
		ContactID:  cID,
		VerifiedBy: agentID,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out.PlatformLeadID == "" {
		t.Error("expected non-empty PlatformLeadID")
	}

	// Assert the contact transitioned + lead was created.
	loaded, _ := contacts.GetByID(t.Context(), cID)
	if loaded.State() != unverifiedcontact.StateVerified {
		t.Errorf("contact state=%q want verified", loaded.State())
	}
	if _, err := leads.GetByID(t.Context(), out.PlatformLeadID); err != nil {
		t.Errorf("expected lead persisted, got %v", err)
	}

	// Assert outbox got LeadVerifiedV1.
	if len(outbox.Events) != 1 {
		t.Fatalf("expected 1 outbox event, got %d", len(outbox.Events))
	}
	ev, ok := outbox.Events[0].(integrationevents.LeadVerifiedV1)
	if !ok {
		t.Fatalf("expected LeadVerifiedV1, got %T", outbox.Events[0])
	}
	if ev.LeadSnapshot.ContactName != "Test Pharma" {
		t.Errorf("snapshot.ContactName=%q", ev.LeadSnapshot.ContactName)
	}
	if ev.LeadSnapshot.MobileE164 != "+919876543210" {
		t.Errorf("snapshot.MobileE164=%q", ev.LeadSnapshot.MobileE164)
	}
}

func TestVerifyUnverifiedContact_PromoteFromNew(t *testing.T) {
	// Caller skipped the call-log step; handler promotes New → InCall
	// so MarkVerified's guard passes.
	t.Parallel()

	contacts := platformtest.NewFakeUnverifiedContactRepository()
	leads := platformtest.NewFakePlatformLeadRepository()
	outbox := platformtest.NewFakeOutbox()
	uow := platformtest.NewFakeUnitOfWork()

	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	cID := unverifiedcontact.ID(ids.NewV7().String())
	c, _ := unverifiedcontact.New(cID, sampleForm(t), agentID, nowFunc())
	_ = contacts.Add(t.Context(), c) // arch-test:ignore-err — fake repo Add cannot fail by construction

	h := command.NewVerifyUnverifiedContactHandler(uow, contacts, leads, outbox, nowFunc, func() platformlead.ID { return platformlead.ID(ids.NewV7().String()) })
	_, err := h.Handle(t.Context(), command.VerifyUnverifiedContactCommand{
		ContactID:  cID,
		VerifiedBy: agentID,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
}

func TestVerifyUnverifiedContact_ContactNotFound(t *testing.T) {
	t.Parallel()

	contacts := platformtest.NewFakeUnverifiedContactRepository()
	leads := platformtest.NewFakePlatformLeadRepository()
	outbox := platformtest.NewFakeOutbox()
	uow := platformtest.NewFakeUnitOfWork()

	h := command.NewVerifyUnverifiedContactHandler(uow, contacts, leads, outbox, nowFunc, func() platformlead.ID { return platformlead.ID(ids.NewV7().String()) })
	_, err := h.Handle(t.Context(), command.VerifyUnverifiedContactCommand{
		ContactID:  unverifiedcontact.ID("01900000-0000-7000-8000-000000000999"),
		VerifiedBy: unverifiedcontact.MembershipID(ids.NewV7().String()),
	})
	if err == nil {
		t.Fatal("expected ErrContactNotFound, got nil")
	}
	if !errors.Is(err, command.ErrContactNotFound) {
		t.Errorf("expected ErrContactNotFound, got %v", err)
	}
}

// TestVerifyUnverifiedContact_AlreadyVerified_Idempotent: re-verifying a
// Verified contact must not spawn a second PlatformLead. H11 —
// review-pass.
func TestVerifyUnverifiedContact_AlreadyVerified_Idempotent(t *testing.T) {
	t.Parallel()

	contacts := platformtest.NewFakeUnverifiedContactRepository()
	leads := platformtest.NewFakePlatformLeadRepository()
	outbox := platformtest.NewFakeOutbox()
	uow := platformtest.NewFakeUnitOfWork()

	// Seed: a contact that's already InCall.
	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	cID := unverifiedcontact.ID(ids.NewV7().String())
	c, err := unverifiedcontact.New(cID, sampleForm(t), agentID, nowFunc())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := c.StartCall(nowFunc()); err != nil {
		t.Fatalf("seed start call: %v", err)
	}
	if err := contacts.Add(t.Context(), c); err != nil {
		t.Fatalf("seed add: %v", err)
	}

	h := command.NewVerifyUnverifiedContactHandler(uow, contacts, leads, outbox, nowFunc, func() platformlead.ID { return platformlead.ID(ids.NewV7().String()) })

	// First verify → success, lead created.
	out1, err := h.Handle(t.Context(), command.VerifyUnverifiedContactCommand{
		ContactID:  cID,
		VerifiedBy: agentID,
	})
	if err != nil {
		t.Fatalf("first verify: %v", err)
	}

	// Second verify on the now-Verified contact must not leak a second
	// lead: expect an idempotent success or a typed sentinel.
	_, err = h.Handle(t.Context(), command.VerifyUnverifiedContactCommand{
		ContactID:  cID,
		VerifiedBy: agentID,
	})
	// Success or the typed sentinel is fine; a second lead is not.
	if err != nil && !errors.Is(err, command.ErrContactAlreadyTerminal) {
		t.Fatalf("second verify: got %v; want nil OR ErrContactAlreadyTerminal", err)
	}

	if len(leads.Store) != 1 {
		t.Errorf("expected exactly 1 lead row after double verify, got %d", len(leads.Store))
	}
	if _, ok := leads.Store[out1.PlatformLeadID]; !ok {
		t.Errorf("first lead missing from store")
	}
}

// TestVerifyUnverifiedContact_AlreadyRejected_Refused: verify after a
// terminal Reject must fail, not flip to Verified. H11 — review-pass.
func TestVerifyUnverifiedContact_AlreadyRejected_Refused(t *testing.T) {
	t.Parallel()

	contacts := platformtest.NewFakeUnverifiedContactRepository()
	leads := platformtest.NewFakePlatformLeadRepository()
	outbox := platformtest.NewFakeOutbox()
	uow := platformtest.NewFakeUnitOfWork()

	// Seed: a contact that's already InCall + Rejected.
	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	cID := unverifiedcontact.ID(ids.NewV7().String())
	c, err := unverifiedcontact.New(cID, sampleForm(t), agentID, nowFunc())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := c.StartCall(nowFunc()); err != nil {
		t.Fatalf("seed start call: %v", err)
	}
	if err := c.MarkRejected("test data", agentID, nowFunc()); err != nil {
		t.Fatalf("seed reject: %v", err)
	}
	if err := contacts.Add(t.Context(), c); err != nil {
		t.Fatalf("seed add: %v", err)
	}

	h := command.NewVerifyUnverifiedContactHandler(uow, contacts, leads, outbox, nowFunc, func() platformlead.ID { return platformlead.ID(ids.NewV7().String()) })
	_, err = h.Handle(t.Context(), command.VerifyUnverifiedContactCommand{
		ContactID:  cID,
		VerifiedBy: agentID,
	})
	if !errors.Is(err, command.ErrContactAlreadyTerminal) {
		t.Fatalf("expected ErrContactAlreadyTerminal (verify-after-reject), got %v", err)
	}
	if len(leads.Store) != 0 {
		t.Errorf("expected no lead created on verify-after-reject, got %d", len(leads.Store))
	}
}
