package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/platform/app/command"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
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
	uow := platformtest.FakeUnitOfWork{}

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
	if err := contacts.Add(context.Background(), c); err != nil {
		t.Fatalf("seed add: %v", err)
	}

	h := command.NewVerifyUnverifiedContactHandler(uow, contacts, leads, outbox, nowFunc)
	out, err := h.Handle(context.Background(), command.VerifyUnverifiedContactCommand{
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
	loaded, _ := contacts.GetByID(context.Background(), cID)
	if loaded.State() != unverifiedcontact.StateVerified {
		t.Errorf("contact state=%q want verified", loaded.State())
	}
	if _, err := leads.GetByID(context.Background(), out.PlatformLeadID); err != nil {
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
	// Even when the caller skips the call-log step, the handler promotes
	// New → InCall first so MarkVerified's guard passes.
	t.Parallel()

	contacts := platformtest.NewFakeUnverifiedContactRepository()
	leads := platformtest.NewFakePlatformLeadRepository()
	outbox := platformtest.NewFakeOutbox()
	uow := platformtest.FakeUnitOfWork{}

	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	cID := unverifiedcontact.ID(ids.NewV7().String())
	c, _ := unverifiedcontact.New(cID, sampleForm(t), agentID, nowFunc())
	_ = contacts.Add(context.Background(), c)

	h := command.NewVerifyUnverifiedContactHandler(uow, contacts, leads, outbox, nowFunc)
	_, err := h.Handle(context.Background(), command.VerifyUnverifiedContactCommand{
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
	uow := platformtest.FakeUnitOfWork{}

	h := command.NewVerifyUnverifiedContactHandler(uow, contacts, leads, outbox, nowFunc)
	_, err := h.Handle(context.Background(), command.VerifyUnverifiedContactCommand{
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
