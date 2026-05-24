package unverifiedcontact_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
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
		HasGst:         true,
		GstNumber:      "27AABCU9603R1ZV",
		HasPan:         true,
		PanNumber:      "AABCU9603R",
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

var (
	now     = time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	agentID = unverifiedcontact.MembershipID("01900000-0000-7000-8000-000000000001")
)

func TestNew_HappyPath(t *testing.T) {
	t.Parallel()
	c, err := unverifiedcontact.New(
		unverifiedcontact.ID("01900000-0000-7000-8000-000000000010"),
		sampleForm(t), agentID, now,
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if c.State() != unverifiedcontact.StateNew {
		t.Errorf("state=%q want %q", c.State(), unverifiedcontact.StateNew)
	}
	evs := c.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if _, ok := evs[0].(unverifiedcontact.CreatedEvent); !ok {
		t.Errorf("expected CreatedEvent, got %T", evs[0])
	}
}

func TestNew_RejectsZeroID(t *testing.T) {
	t.Parallel()
	_, err := unverifiedcontact.New("", sampleForm(t), agentID, now)
	if !errors.Is(err, unverifiedcontact.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestNew_RejectsZeroAgent(t *testing.T) {
	t.Parallel()
	_, err := unverifiedcontact.New(
		unverifiedcontact.ID("01900000-0000-7000-8000-000000000010"),
		sampleForm(t), "", now,
	)
	if !errors.Is(err, unverifiedcontact.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func freshContact(t *testing.T) *unverifiedcontact.UnverifiedContact {
	t.Helper()
	c, err := unverifiedcontact.New(
		unverifiedcontact.ID("01900000-0000-7000-8000-000000000010"),
		sampleForm(t), agentID, now,
	)
	if err != nil {
		t.Fatalf("freshContact: %v", err)
	}
	// drain New event
	_ = c.PullEvents()
	return c
}

func TestStartCall_NewToInCall(t *testing.T) {
	t.Parallel()
	c := freshContact(t)
	if err := c.StartCall(now.Add(time.Minute)); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if c.State() != unverifiedcontact.StateInCall {
		t.Errorf("state=%q", c.State())
	}
	evs := c.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if _, ok := evs[0].(unverifiedcontact.CallStartedEvent); !ok {
		t.Errorf("expected CallStartedEvent, got %T", evs[0])
	}
}

func TestStartCall_AlreadyInCall_Idempotent(t *testing.T) {
	t.Parallel()
	c := freshContact(t)
	_ = c.StartCall(now) // arch-test:ignore-err — domain test seed
	_ = c.PullEvents()
	if err := c.StartCall(now.Add(time.Minute)); err != nil {
		t.Errorf("idempotent expected, got %v", err)
	}
	if evs := c.PullEvents(); len(evs) != 0 {
		t.Errorf("expected no events on idempotent retry, got %d", len(evs))
	}
}

func TestStartCall_FromVerified_Rejected(t *testing.T) {
	t.Parallel()
	c := freshContact(t)
	_ = c.StartCall(now) // arch-test:ignore-err — domain test seed
	_ = c.MarkVerified("01900000-0000-7000-8000-000000000020", agentID, now.Add(time.Minute)) // arch-test:ignore-err — domain test seed
	_ = c.PullEvents()
	err := c.StartCall(now.Add(2 * time.Minute))
	if !errors.Is(err, unverifiedcontact.ErrInvalid) {
		t.Errorf("expected ErrInvalid from terminal state, got %v", err)
	}
}

func TestMarkVerified_HappyPath(t *testing.T) {
	t.Parallel()
	c := freshContact(t)
	_ = c.StartCall(now) // arch-test:ignore-err — domain test seed
	_ = c.PullEvents()
	at := now.Add(2 * time.Minute)
	leadID := "01900000-0000-7000-8000-000000000020"
	if err := c.MarkVerified(leadID, agentID, at); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if c.State() != unverifiedcontact.StateVerified {
		t.Errorf("state=%q", c.State())
	}
	if c.PlatformLeadID() != leadID {
		t.Errorf("PlatformLeadID=%q", c.PlatformLeadID())
	}
	evs := c.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ve, ok := evs[0].(unverifiedcontact.VerifiedEvent)
	if !ok {
		t.Fatalf("expected VerifiedEvent, got %T", evs[0])
	}
	if ve.PlatformLeadID != leadID {
		t.Errorf("event.PlatformLeadID=%q", ve.PlatformLeadID)
	}
}

func TestMarkVerified_RequiresInCallState(t *testing.T) {
	t.Parallel()
	c := freshContact(t)
	// from New
	err := c.MarkVerified("01900000-0000-7000-8000-000000000020", agentID, now.Add(time.Minute))
	if !errors.Is(err, unverifiedcontact.ErrInvalid) {
		t.Errorf("expected ErrInvalid (not in_call), got %v", err)
	}
}

func TestMarkVerified_RequiresLeadID(t *testing.T) {
	t.Parallel()
	c := freshContact(t)
	_ = c.StartCall(now) // arch-test:ignore-err — domain test seed
	err := c.MarkVerified("", agentID, now.Add(time.Minute))
	if !errors.Is(err, unverifiedcontact.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestMarkVerified_Idempotent(t *testing.T) {
	t.Parallel()
	c := freshContact(t)
	_ = c.StartCall(now) // arch-test:ignore-err — domain test seed
	at := now.Add(2 * time.Minute)
	_ = c.MarkVerified("01900000-0000-7000-8000-000000000020", agentID, at) // arch-test:ignore-err — domain test seed
	_ = c.PullEvents()
	if err := c.MarkVerified("01900000-0000-7000-8000-000000000020", agentID, at.Add(time.Minute)); err != nil {
		t.Errorf("idempotent expected, got %v", err)
	}
	if evs := c.PullEvents(); len(evs) != 0 {
		t.Errorf("expected no events on idempotent retry, got %d", len(evs))
	}
}

func TestMarkRejected_HappyPath(t *testing.T) {
	t.Parallel()
	c := freshContact(t)
	_ = c.StartCall(now) // arch-test:ignore-err — domain test seed
	_ = c.PullEvents()
	at := now.Add(2 * time.Minute)
	if err := c.MarkRejected("Wrong number", agentID, at); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if c.State() != unverifiedcontact.StateRejected {
		t.Errorf("state=%q", c.State())
	}
	if c.RejectionReason() != "Wrong number" {
		t.Errorf("reason=%q", c.RejectionReason())
	}
	evs := c.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	if _, ok := evs[0].(unverifiedcontact.RejectedEvent); !ok {
		t.Errorf("expected RejectedEvent, got %T", evs[0])
	}
}

func TestMarkRejected_RequiresReason(t *testing.T) {
	t.Parallel()
	c := freshContact(t)
	_ = c.StartCall(now) // arch-test:ignore-err — domain test seed
	err := c.MarkRejected("   ", agentID, now.Add(time.Minute))
	if !errors.Is(err, unverifiedcontact.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestMarkBusy_HappyPath(t *testing.T) {
	t.Parallel()
	c := freshContact(t)
	_ = c.StartCall(now) // arch-test:ignore-err — domain test seed
	_ = c.PullEvents()
	cbAt := now.Add(time.Hour)
	cbEnd := cbAt.Add(30 * time.Minute)
	if err := c.MarkBusy(cbAt, cbEnd, now); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if c.State() != unverifiedcontact.StateBusy {
		t.Errorf("state=%q", c.State())
	}
	if !c.BusyCallbackAt().Equal(cbAt) {
		t.Errorf("BusyCallbackAt=%v want %v", c.BusyCallbackAt(), cbAt)
	}
}

func TestMarkBusy_RejectsPastCallback(t *testing.T) {
	t.Parallel()
	c := freshContact(t)
	_ = c.StartCall(now) // arch-test:ignore-err — domain test seed
	err := c.MarkBusy(now.Add(-time.Hour), now.Add(-30*time.Minute), now)
	if !errors.Is(err, unverifiedcontact.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestMarkBusy_RejectsEndBeforeStart(t *testing.T) {
	t.Parallel()
	c := freshContact(t)
	_ = c.StartCall(now) // arch-test:ignore-err — domain test seed
	err := c.MarkBusy(now.Add(time.Hour), now.Add(time.Minute), now)
	if !errors.Is(err, unverifiedcontact.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestStartCall_FromBusy_ReentersInCall(t *testing.T) {
	t.Parallel()
	c := freshContact(t)
	_ = c.StartCall(now) // arch-test:ignore-err — domain test seed
	_ = c.MarkBusy(now.Add(time.Hour), now.Add(2*time.Hour), now) // arch-test:ignore-err — domain test seed
	_ = c.PullEvents()
	if err := c.StartCall(now.Add(time.Hour + time.Minute)); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if c.State() != unverifiedcontact.StateInCall {
		t.Errorf("state=%q", c.State())
	}
	if !c.BusyCallbackAt().IsZero() {
		t.Errorf("expected callback cleared on retry, got %v", c.BusyCallbackAt())
	}
}

func TestUnmarshalFromDB_RoundTrip(t *testing.T) {
	t.Parallel()
	snap := unverifiedcontact.Snapshot{
		ID:                    unverifiedcontact.ID("01900000-0000-7000-8000-000000000010"),
		Form:                  sampleForm(t),
		State:                 unverifiedcontact.StateVerified,
		PlatformLeadID:        "01900000-0000-7000-8000-000000000020",
		CreatedAt:             now,
		CreatedByMembershipID: agentID,
		VerifiedAt:            now.Add(time.Minute),
		VerifiedByMembershipID: agentID,
	}
	c := unverifiedcontact.UnmarshalFromDB(snap)
	if c.State() != unverifiedcontact.StateVerified {
		t.Errorf("state=%q", c.State())
	}
	if c.PlatformLeadID() != snap.PlatformLeadID {
		t.Errorf("PlatformLeadID round-trip failed")
	}
}
