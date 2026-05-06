package tenant_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func mustEmail(t *testing.T, raw string) email.Address {
	t.Helper()
	e, err := email.New(raw)
	if err != nil {
		t.Fatalf("mustEmail(%q): %v", raw, err)
	}
	return e
}

func mustSlug(t *testing.T, raw string) slug.Slug {
	t.Helper()
	s, err := slug.New(raw)
	if err != nil {
		t.Fatalf("mustSlug(%q): %v", raw, err)
	}
	return s
}

func newID(t *testing.T) tenant.ID {
	t.Helper()
	return tenant.ID(ids.NewV7().String())
}

// ----- Factory: NewTenant ---------------------------------------------------

func TestNewTenant_AcceptsValidInputs(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	id := newID(t)
	s := mustSlug(t, "acme-pharma")
	e := mustEmail(t, "admin@acme.example")

	tn, err := tenant.New(id, s, "Acme Pharma Pvt Ltd", "Acme Pharma", e)
	if err != nil {
		t.Fatalf("New: unexpected error %v", err)
	}
	if tn == nil {
		t.Fatal("New: returned nil")
	}
	if tn.ID() != id {
		t.Errorf("ID() = %v, want %v", tn.ID(), id)
	}
	if !tn.Slug().Equal(s) {
		t.Errorf("Slug() = %v, want %v", tn.Slug(), s)
	}
	if tn.LegalName() != "Acme Pharma Pvt Ltd" {
		t.Errorf("LegalName() = %q", tn.LegalName())
	}
	if tn.DisplayName() != "Acme Pharma" {
		t.Errorf("DisplayName() = %q", tn.DisplayName())
	}
	if !tn.AdminEmail().Equal(e) {
		t.Errorf("AdminEmail() mismatch")
	}
	if tn.Status() != tenant.StatusPending {
		t.Errorf("Status() = %v, want StatusPending", tn.Status())
	}
	if !tn.CreatedAt().Equal(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("CreatedAt() = %v, want clock.Now()", tn.CreatedAt())
	}
}

func TestNewTenant_EmitsTenantRegisteredEvent(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	id := newID(t)
	tn, err := tenant.New(id, mustSlug(t, "acme"), "Acme", "Acme", mustEmail(t, "a@b.io"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	events := tn.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	reg, ok := events[0].(tenant.RegisteredEvent)
	if !ok {
		t.Fatalf("event[0] type = %T, want RegisteredEvent", events[0])
	}
	if reg.TenantID != id {
		t.Errorf("event TenantID = %v, want %v", reg.TenantID, id)
	}
}

func TestNewTenant_RejectsZeroID(t *testing.T) {
	t.Parallel()
	_, err := tenant.New(tenant.ID(""), mustSlug(t, "acme"), "L", "D", mustEmail(t, "a@b.io"))
	if err == nil {
		t.Fatal("expected error on zero ID")
	}
	if errs.KindOf(err) != errs.KindInvalidInput {
		t.Errorf("Kind = %v, want KindInvalidInput", errs.KindOf(err))
	}
}

func TestNewTenant_RejectsZeroSlug(t *testing.T) {
	t.Parallel()
	_, err := tenant.New(newID(t), slug.Slug{}, "L", "D", mustEmail(t, "a@b.io"))
	if err == nil {
		t.Fatal("expected error on zero slug")
	}
}

func TestNewTenant_RejectsEmptyLegalName(t *testing.T) {
	t.Parallel()
	_, err := tenant.New(newID(t), mustSlug(t, "acme"), "", "D", mustEmail(t, "a@b.io"))
	if err == nil {
		t.Fatal("expected error on empty legal name")
	}
}

func TestNewTenant_RejectsEmptyDisplayName(t *testing.T) {
	t.Parallel()
	_, err := tenant.New(newID(t), mustSlug(t, "acme"), "L", "", mustEmail(t, "a@b.io"))
	if err == nil {
		t.Fatal("expected error on empty display name")
	}
}

func TestNewTenant_RejectsLegalNameTooLong(t *testing.T) {
	t.Parallel()
	long := string(make([]byte, 300))
	_, err := tenant.New(newID(t), mustSlug(t, "acme"), long, "D", mustEmail(t, "a@b.io"))
	if err == nil {
		t.Fatal("expected error on overlong legal name")
	}
}

func TestNewTenant_RejectsZeroEmail(t *testing.T) {
	t.Parallel()
	_, err := tenant.New(newID(t), mustSlug(t, "acme"), "L", "D", email.Address{})
	if err == nil {
		t.Fatal("expected error on zero email")
	}
}

// ----- State transitions: Activate ------------------------------------------

// ----- UpdateProfile --------------------------------------------------------

func TestUpdateProfile_ChangesNamesAndEmits(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))

	tn := newPendingTenant(t)
	_ = tn.PullEvents()

	if err := tn.UpdateProfile("New Acme Pharma Pvt Ltd", "Acme New"); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if tn.LegalName() != "New Acme Pharma Pvt Ltd" {
		t.Errorf("LegalName = %q", tn.LegalName())
	}
	if tn.DisplayName() != "Acme New" {
		t.Errorf("DisplayName = %q", tn.DisplayName())
	}

	events := tn.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(tenant.ProfileUpdatedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want ProfileUpdatedEvent", events[0])
	}
	if ev.OldLegalName != "Acme Pharma" || ev.NewLegalName != "New Acme Pharma Pvt Ltd" {
		t.Errorf("OLD/NEW legal mismatch: %+v", ev)
	}
	if ev.OldDisplayName != "Acme" || ev.NewDisplayName != "Acme New" {
		t.Errorf("OLD/NEW display mismatch: %+v", ev)
	}
}

func TestUpdateProfile_NoOp_WhenUnchanged(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newPendingTenant(t)
	_ = tn.PullEvents()
	if err := tn.UpdateProfile(tn.LegalName(), tn.DisplayName()); err != nil {
		t.Fatalf("UpdateProfile noop: %v", err)
	}
	if got := tn.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events, got %d", len(got))
	}
}

func TestUpdateProfile_RejectsEmptyAndOverlong(t *testing.T) {
	t.Cleanup(clock.Reset)
	cases := []struct {
		name        string
		legalName   string
		displayName string
	}{
		{"empty legal", "", "Display"},
		{"empty display", "Legal", ""},
		{"whitespace legal", "   ", "Display"},
		{"whitespace display", "Legal", "  "},
		{"legal too long", string(make([]byte, 201)), "Display"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tn := newPendingTenant(t)
			err := tn.UpdateProfile(tc.legalName, tc.displayName)
			if !errors.Is(err, tenant.ErrInvalid) {
				t.Errorf("expected ErrInvalid, got %v", err)
			}
		})
	}
}

func TestUpdateProfile_AllowedOnSuspendedTenant(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newSuspendedTenant(t)
	_ = tn.PullEvents()
	if err := tn.UpdateProfile("Renamed Pharma", "Renamed"); err != nil {
		t.Errorf("UpdateProfile on suspended tenant: %v", err)
	}
}

func TestActivate_FromPending_TransitionsToActive(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	tn := newPendingTenant(t)
	_ = tn.PullEvents()

	clock.Set(time.Date(2026, 5, 5, 13, 0, 0, 0, time.UTC))
	if err := tn.Activate(); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if tn.Status() != tenant.StatusActive {
		t.Errorf("Status() = %v, want StatusActive", tn.Status())
	}
	if !tn.ActivatedAt().Equal(time.Date(2026, 5, 5, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("ActivatedAt() = %v, want 13:00", tn.ActivatedAt())
	}

	events := tn.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if _, ok := events[0].(tenant.ActivatedEvent); !ok {
		t.Errorf("event[0] = %T, want ActivatedEvent", events[0])
	}
}

func TestActivate_FromActive_NoOp(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	tn := newActiveTenant(t)
	_ = tn.PullEvents()

	if err := tn.Activate(); err != nil {
		t.Fatalf("Activate (idempotent): %v", err)
	}
	if got := tn.PullEvents(); len(got) != 0 {
		t.Errorf("expected no event on idempotent activate, got %d", len(got))
	}
}

func TestActivate_FromSuspended_TransitionsToActive(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	tn := newSuspendedTenant(t)
	_ = tn.PullEvents()

	if err := tn.Activate(); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if tn.Status() != tenant.StatusActive {
		t.Errorf("Status() = %v, want StatusActive", tn.Status())
	}
	events := tn.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if _, ok := events[0].(tenant.ActivatedEvent); !ok {
		t.Errorf("event[0] = %T, want ActivatedEvent", events[0])
	}
}

// ----- State transitions: Suspend -------------------------------------------

func TestSuspend_FromActive_TransitionsToSuspended(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	tn := newActiveTenant(t)
	_ = tn.PullEvents()

	clock.Set(time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC))
	if err := tn.Suspend("payment overdue 30 days"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if tn.Status() != tenant.StatusSuspended {
		t.Errorf("Status() = %v, want StatusSuspended", tn.Status())
	}
	if !tn.SuspendedAt().Equal(time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("SuspendedAt() = %v, want 14:00", tn.SuspendedAt())
	}

	events := tn.PullEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	susp, ok := events[0].(tenant.SuspendedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want SuspendedEvent", events[0])
	}
	if susp.Reason != "payment overdue 30 days" {
		t.Errorf("event reason = %q", susp.Reason)
	}
}

func TestSuspend_FromPending_TransitionsToSuspended(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	tn := newPendingTenant(t)
	_ = tn.PullEvents()

	if err := tn.Suspend("flagged at signup"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if tn.Status() != tenant.StatusSuspended {
		t.Errorf("Status() = %v, want StatusSuspended", tn.Status())
	}
}

func TestSuspend_FromSuspended_NoOp(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	tn := newSuspendedTenant(t)
	_ = tn.PullEvents()

	if err := tn.Suspend("repeat"); err != nil {
		t.Fatalf("Suspend (idempotent): %v", err)
	}
	if got := tn.PullEvents(); len(got) != 0 {
		t.Errorf("expected no event on idempotent suspend, got %d", len(got))
	}
}

func TestSuspend_RejectsEmptyReason(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	tn := newActiveTenant(t)
	if err := tn.Suspend(""); err == nil {
		t.Fatal("expected error on empty reason")
	}
}

// ----- PullEvents -----------------------------------------------------------

func TestPullEvents_DrainsAndClears(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))

	tn := newPendingTenant(t)
	first := tn.PullEvents()
	second := tn.PullEvents()

	if len(first) != 1 {
		t.Fatalf("first PullEvents: got %d, want 1", len(first))
	}
	if len(second) != 0 {
		t.Fatalf("second PullEvents: got %d, want 0 (drained)", len(second))
	}
}

// ----- Re-hydration: UnmarshalFromDB ----------------------------------------

func TestUnmarshalFromDB_DoesNotValidate(t *testing.T) {
	t.Parallel()
	// Re-hydration accepts data that the factory would reject.
	// Reason: data was valid when stored; re-validating could corrupt
	// history if invariants tightened in code.
	tn := tenant.UnmarshalFromDB(tenant.Snapshot{
		ID:          tenant.ID("not-a-real-uuid"), // factory would reject
		Slug:        slug.Slug{},                  // factory would reject
		LegalName:   "",                           // factory would reject
		DisplayName: "",                           // factory would reject
		AdminEmail:  email.Address{},              // factory would reject
		Status:      tenant.StatusActive,
		CreatedAt:   time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if tn == nil {
		t.Fatal("UnmarshalFromDB returned nil")
	}
	if tn.Status() != tenant.StatusActive {
		t.Errorf("Status preserved? got %v", tn.Status())
	}
}

func TestUnmarshalFromDB_DoesNotEmitEvents(t *testing.T) {
	t.Parallel()
	tn := tenant.UnmarshalFromDB(tenant.Snapshot{
		ID:          tenant.ID("019df708-f642-7f66-b73b-c7919f2447cb"),
		Slug:        slug.Slug{},
		LegalName:   "Acme",
		DisplayName: "Acme",
		AdminEmail:  email.Address{},
		Status:      tenant.StatusActive,
		CreatedAt:   time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if got := tn.PullEvents(); len(got) != 0 {
		t.Errorf("re-hydration emitted %d events, want 0", len(got))
	}
}

// ----- Sentinel errors -----------------------------------------------------

func TestErrInvalid_IsClassified(t *testing.T) {
	t.Parallel()
	_, err := tenant.New(tenant.ID(""), mustSlug(t, "acme"), "L", "D", mustEmail(t, "a@b.io"))
	if !errors.Is(err, tenant.ErrInvalid) {
		t.Fatalf("expected errors.Is(_, ErrInvalid), got %v", err)
	}
}

// ----- Helpers --------------------------------------------------------------

func newPendingTenant(t *testing.T) *tenant.Tenant {
	t.Helper()
	tn, err := tenant.New(newID(t), mustSlug(t, "acme"), "Acme Pharma", "Acme", mustEmail(t, "a@acme.io"))
	if err != nil {
		t.Fatalf("newPendingTenant: %v", err)
	}
	return tn
}

func newActiveTenant(t *testing.T) *tenant.Tenant {
	t.Helper()
	tn := newPendingTenant(t)
	if err := tn.Activate(); err != nil {
		t.Fatalf("newActiveTenant.Activate: %v", err)
	}
	return tn
}

func newSuspendedTenant(t *testing.T) *tenant.Tenant {
	t.Helper()
	tn := newActiveTenant(t)
	if err := tn.Suspend("test"); err != nil {
		t.Fatalf("newSuspendedTenant.Suspend: %v", err)
	}
	return tn
}
