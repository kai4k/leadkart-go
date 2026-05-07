package tenant_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/common/druglicence"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/gst"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pan"
	"github.com/leadkart/leadkart-go/internal/common/phone"
	"github.com/leadkart/leadkart-go/internal/common/postaladdress"
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

// ----- UpdateStatutory ------------------------------------------------------

func mustGST(t *testing.T, raw string) gst.Number {
	t.Helper()
	g, err := gst.New(raw)
	if err != nil {
		t.Fatalf("gst.New(%q): %v", raw, err)
	}
	return g
}

func mustPAN(t *testing.T, raw string) pan.Number {
	t.Helper()
	p, err := pan.New(raw)
	if err != nil {
		t.Fatalf("pan.New(%q): %v", raw, err)
	}
	return p
}

func mustDL(t *testing.T, raw string) druglicence.Number {
	t.Helper()
	dl, err := druglicence.New(raw)
	if err != nil {
		t.Fatalf("druglicence.New(%q): %v", raw, err)
	}
	return dl
}

func TestNewStatutory_RejectsGSTPANMismatch(t *testing.T) {
	t.Parallel()
	g := mustGST(t, "29ABCPE1234F1Z5") // embedded PAN: ABCPE1234F
	p := mustPAN(t, "ZZZPZ9999G")       // different PAN, P at position 4
	_, err := tenant.NewStatutory(g, p, druglicence.Number{})
	if !errors.Is(err, tenant.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestNewStatutory_AcceptsMatchedGSTPAN(t *testing.T) {
	t.Parallel()
	g := mustGST(t, "29ABCPE1234F1Z5")
	p := mustPAN(t, "ABCPE1234F")
	s, err := tenant.NewStatutory(g, p, druglicence.Number{})
	if err != nil {
		t.Fatalf("NewStatutory: %v", err)
	}
	if !s.GST().Equal(g) {
		t.Error("GST not stored")
	}
	if !s.PAN().Equal(p) {
		t.Error("PAN not stored")
	}
}

func TestUpdateStatutory_FirstDeclaration_EmitsEvent(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))

	tn := newActiveTenant(t)
	_ = tn.PullEvents()

	s, _ := tenant.NewStatutory(
		mustGST(t, "29ABCPE1234F1Z5"),
		mustPAN(t, "ABCPE1234F"),
		mustDL(t, "KA-W-22B-12345"),
	)
	if err := tn.UpdateStatutory(s); err != nil {
		t.Fatalf("UpdateStatutory: %v", err)
	}
	if !tn.Statutory().Equal(s) {
		t.Error("Statutory not stored")
	}

	events := tn.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(tenant.StatutoryUpdatedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want StatutoryUpdatedEvent", events[0])
	}
	if !ev.OldStatutory.IsZero() {
		t.Error("OldStatutory should be zero on first declaration")
	}
	if !ev.NewStatutory.Equal(s) {
		t.Error("NewStatutory mismatch")
	}
}

func TestUpdateStatutory_NoOp_WhenUnchanged(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newActiveTenant(t)
	s, _ := tenant.NewStatutory(
		mustGST(t, "29ABCPE1234F1Z5"),
		mustPAN(t, "ABCPE1234F"),
		druglicence.Number{},
	)
	_ = tn.UpdateStatutory(s)
	_ = tn.PullEvents()

	if err := tn.UpdateStatutory(s); err != nil {
		t.Fatalf("UpdateStatutory noop: %v", err)
	}
	if got := tn.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events, got %d", len(got))
	}
}

func TestUpdateStatutory_AllowedInAllNonTerminalStatuses(t *testing.T) {
	t.Cleanup(clock.Reset)
	s, _ := tenant.NewStatutory(mustGST(t, "29ABCPE1234F1Z5"), mustPAN(t, "ABCPE1234F"), druglicence.Number{})
	for _, factory := range []struct {
		name string
		fn   func(*testing.T) *tenant.Tenant
	}{
		{"pending", newPendingTenant},
		{"active", newActiveTenant},
		{"suspended", newSuspendedTenant},
	} {
		t.Run(factory.name, func(t *testing.T) {
			tn := factory.fn(t)
			if err := tn.UpdateStatutory(s); err != nil {
				t.Errorf("UpdateStatutory on %s tenant: %v", factory.name, err)
			}
		})
	}
}

func TestUpdateStatutory_RejectedOnDeletedTenant(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newPendingTenant(t)
	_ = tn.HardDelete()
	s, _ := tenant.NewStatutory(mustGST(t, "29ABCPE1234F1Z5"), mustPAN(t, "ABCPE1234F"), druglicence.Number{})
	if err := tn.UpdateStatutory(s); !errors.Is(err, tenant.ErrInvalid) {
		t.Errorf("expected ErrInvalid on deleted tenant, got %v", err)
	}
}

// ----- UpdateAdminContact ---------------------------------------------------

func mustPhone(t *testing.T, raw string) phone.Number {
	t.Helper()
	p, err := phone.New(raw)
	if err != nil {
		t.Fatalf("phone.New(%q): %v", raw, err)
	}
	return p
}

func mustAddress(t *testing.T) postaladdress.Address {
	t.Helper()
	a, err := postaladdress.New("123 MG Road", "Bangalore", "Bangalore Urban", "Karnataka", "KA", "560001")
	if err != nil {
		t.Fatalf("postaladdress.New: %v", err)
	}
	return a
}

func TestUpdateAdminContact_FirstDeclaration_EmitsEvent(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))

	tn := newActiveTenant(t)
	_ = tn.PullEvents()

	c := tenant.NewAdminContact(mustPhone(t, "+919876543210"), mustAddress(t))
	if err := tn.UpdateAdminContact(c); err != nil {
		t.Fatalf("UpdateAdminContact: %v", err)
	}
	if !tn.AdminContact().Equal(c) {
		t.Error("AdminContact not stored")
	}

	events := tn.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(tenant.AdminContactUpdatedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want AdminContactUpdatedEvent", events[0])
	}
	if !ev.OldAdminContact.IsZero() {
		t.Error("OldAdminContact should be zero on first declaration")
	}
	if !ev.NewAdminContact.Equal(c) {
		t.Error("NewAdminContact mismatch")
	}
}

func TestUpdateAdminContact_NoOp_WhenUnchanged(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newActiveTenant(t)
	c := tenant.NewAdminContact(mustPhone(t, "+919876543210"), mustAddress(t))
	_ = tn.UpdateAdminContact(c)
	_ = tn.PullEvents()

	if err := tn.UpdateAdminContact(c); err != nil {
		t.Fatalf("UpdateAdminContact noop: %v", err)
	}
	if got := tn.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events, got %d", len(got))
	}
}

func TestUpdateAdminContact_RejectedOnDeletedTenant(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newPendingTenant(t)
	_ = tn.HardDelete()
	c := tenant.NewAdminContact(mustPhone(t, "+919876543210"), mustAddress(t))
	if err := tn.UpdateAdminContact(c); !errors.Is(err, tenant.ErrInvalid) {
		t.Errorf("expected ErrInvalid on deleted tenant, got %v", err)
	}
}

func TestUpdateAdminContact_PartialUpdate(t *testing.T) {
	// Only phone, no address (address is zero) — still a valid contact
	// declaration; tenant's address is just "not declared".
	t.Cleanup(clock.Reset)
	tn := newActiveTenant(t)
	c := tenant.NewAdminContact(mustPhone(t, "+919876543210"), postaladdress.Address{})
	if err := tn.UpdateAdminContact(c); err != nil {
		t.Fatalf("UpdateAdminContact phone-only: %v", err)
	}
	if !tn.AdminContact().Phone().Equal(mustPhone(t, "+919876543210")) {
		t.Error("phone not stored")
	}
	if !tn.AdminContact().Address().IsZero() {
		t.Error("address should be zero")
	}
}

// ----- UpdateSettings -------------------------------------------------------

func TestNewPasswordPolicy_AcceptsValid(t *testing.T) {
	t.Parallel()
	p, err := tenant.NewPasswordPolicy(12, true, true, true, true, 5, 15)
	if err != nil {
		t.Fatalf("NewPasswordPolicy: %v", err)
	}
	if p.MinLength() != 12 {
		t.Errorf("MinLength = %d", p.MinLength())
	}
	if p.MaxFailedAttempts() != 5 {
		t.Errorf("MaxFailedAttempts = %d", p.MaxFailedAttempts())
	}
	if !p.RequireUppercase() || !p.RequireLowercase() || !p.RequireDigit() || !p.RequireSymbol() {
		t.Error("char-class flags not set")
	}
}

func TestNewPasswordPolicy_RejectsBelowFloors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		minLength         int
		maxFailedAttempts int
		lockoutMinutes    int
	}{
		{"minLength below NIST floor", 7, 5, 15},
		{"minLength too high", 200, 5, 15},
		{"maxFailedAttempts too low", 12, 2, 15},
		{"maxFailedAttempts too high", 12, 100, 15},
		{"lockout negative", 12, 5, -1},
		{"lockout > 24h", 12, 5, 1500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tenant.NewPasswordPolicy(tc.minLength, true, true, true, true, tc.maxFailedAttempts, tc.lockoutMinutes)
			if !errors.Is(err, tenant.ErrInvalid) {
				t.Errorf("expected ErrInvalid, got %v", err)
			}
		})
	}
}

func TestDefaultPasswordPolicy_MeetsAllFloors(t *testing.T) {
	t.Parallel()
	d := tenant.DefaultPasswordPolicy()
	if d.MinLength() < 8 || d.MaxFailedAttempts() < 3 || d.LockoutMinutes() < 0 {
		t.Errorf("default policy below floors: %+v", d)
	}
	if d.IsZero() {
		t.Error("default should not be zero")
	}
}

func TestUpdateSettings_FirstDeclaration_EmitsEvent(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))

	tn := newActiveTenant(t)
	_ = tn.PullEvents()

	pol := tenant.DefaultPasswordPolicy()
	s := tenant.NewSettings(pol)
	if err := tn.UpdateSettings(s); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if !tn.Settings().Equal(s) {
		t.Error("Settings not stored")
	}

	events := tn.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(tenant.SettingsUpdatedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want SettingsUpdatedEvent", events[0])
	}
	if !ev.OldSettings.IsZero() {
		t.Error("OldSettings should be zero on first declaration")
	}
	if !ev.NewSettings.Equal(s) {
		t.Error("NewSettings mismatch")
	}
}

func TestUpdateSettings_NoOp_WhenUnchanged(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newActiveTenant(t)
	s := tenant.NewSettings(tenant.DefaultPasswordPolicy())
	_ = tn.UpdateSettings(s)
	_ = tn.PullEvents()
	if err := tn.UpdateSettings(s); err != nil {
		t.Fatalf("UpdateSettings noop: %v", err)
	}
	if got := tn.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events, got %d", len(got))
	}
}

func TestUpdateSettings_RejectedOnDeletedTenant(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newPendingTenant(t)
	_ = tn.HardDelete()
	s := tenant.NewSettings(tenant.DefaultPasswordPolicy())
	if err := tn.UpdateSettings(s); !errors.Is(err, tenant.ErrInvalid) {
		t.Errorf("expected ErrInvalid on deleted, got %v", err)
	}
}

// ----- UpdateDisplayPreferences ---------------------------------------------

func TestNewDisplayPreferences_AcceptsValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		locale string
		tz     string
		fmt    string
		curr   string
	}{
		{"india default", "en-IN", "Asia/Kolkata", "DD-MMM-YYYY", "INR"},
		{"hindi", "hi-IN", "Asia/Kolkata", "DD/MM/YYYY", "INR"},
		{"us", "en-US", "America/New_York", "MM/DD/YYYY", "USD"},
		{"locale no region", "en", "UTC", "YYYY-MM-DD", "USD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := tenant.NewDisplayPreferences(tc.locale, tc.tz, tc.fmt, tc.curr)
			if err != nil {
				t.Fatalf("NewDisplayPreferences(%q,%q,%q,%q): %v", tc.locale, tc.tz, tc.fmt, tc.curr, err)
			}
			if d.Locale() != tc.locale || d.TimeZone() != tc.tz || d.DateFormat() != tc.fmt || d.Currency() != tc.curr {
				t.Errorf("round-trip mismatch: %+v", d)
			}
		})
	}
}

func TestNewDisplayPreferences_RejectsInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		locale string
		tz     string
		fmt    string
		curr   string
	}{
		{"empty locale", "", "Asia/Kolkata", "DD-MMM-YYYY", "INR"},
		{"empty tz", "en-IN", "", "DD-MMM-YYYY", "INR"},
		{"empty format", "en-IN", "Asia/Kolkata", "", "INR"},
		{"empty currency", "en-IN", "Asia/Kolkata", "DD-MMM-YYYY", ""},
		{"locale uppercase primary", "EN-IN", "Asia/Kolkata", "DD-MMM-YYYY", "INR"},
		{"locale region lowercase", "en-in", "Asia/Kolkata", "DD-MMM-YYYY", "INR"},
		{"tz not iana", "en-IN", "Mars/Olympus", "DD-MMM-YYYY", "INR"},
		{"currency lowercase", "en-IN", "Asia/Kolkata", "DD-MMM-YYYY", "inr"},
		{"currency 2 letters", "en-IN", "Asia/Kolkata", "DD-MMM-YYYY", "IN"},
		{"currency 4 letters", "en-IN", "Asia/Kolkata", "DD-MMM-YYYY", "INRX"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := tenant.NewDisplayPreferences(tc.locale, tc.tz, tc.fmt, tc.curr)
			if !errors.Is(err, tenant.ErrInvalid) {
				t.Errorf("expected ErrInvalid, got %v", err)
			}
		})
	}
}

func TestDefaultDisplayPreferences_IsIndiaTuned(t *testing.T) {
	t.Parallel()
	d := tenant.DefaultDisplayPreferences()
	if d.Locale() != "en-IN" || d.TimeZone() != "Asia/Kolkata" || d.Currency() != "INR" {
		t.Errorf("default preferences not India-tuned: %+v", d)
	}
	if d.IsZero() {
		t.Error("default should not be zero")
	}
}

func TestUpdateDisplayPreferences_FirstDeclaration_EmitsEvent(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))

	tn := newActiveTenant(t)
	_ = tn.PullEvents()

	d := tenant.DefaultDisplayPreferences()
	if err := tn.UpdateDisplayPreferences(d); err != nil {
		t.Fatalf("UpdateDisplayPreferences: %v", err)
	}
	if !tn.DisplayPreferences().Equal(d) {
		t.Error("DisplayPreferences not stored")
	}

	events := tn.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(tenant.DisplayPreferencesUpdatedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want DisplayPreferencesUpdatedEvent", events[0])
	}
	if !ev.OldDisplayPreferences.IsZero() {
		t.Error("Old should be zero on first declaration")
	}
	if !ev.NewDisplayPreferences.Equal(d) {
		t.Error("NewDisplayPreferences mismatch")
	}
}

func TestUpdateDisplayPreferences_NoOp_WhenUnchanged(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newActiveTenant(t)
	d := tenant.DefaultDisplayPreferences()
	_ = tn.UpdateDisplayPreferences(d)
	_ = tn.PullEvents()
	if err := tn.UpdateDisplayPreferences(d); err != nil {
		t.Fatalf("UpdateDisplayPreferences noop: %v", err)
	}
	if got := tn.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events, got %d", len(got))
	}
}

func TestUpdateDisplayPreferences_RejectedOnDeletedTenant(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newPendingTenant(t)
	_ = tn.HardDelete()
	d := tenant.DefaultDisplayPreferences()
	if err := tn.UpdateDisplayPreferences(d); !errors.Is(err, tenant.ErrInvalid) {
		t.Errorf("expected ErrInvalid on deleted, got %v", err)
	}
}

// ----- Deletion lifecycle ---------------------------------------------------

func TestMarkForDeletion_FromActive_TransitionsToPendingDeletion(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))

	tn := newActiveTenant(t)
	_ = tn.PullEvents()

	if err := tn.MarkForDeletion("operator-requested-exit"); err != nil {
		t.Fatalf("MarkForDeletion: %v", err)
	}
	if tn.Status() != tenant.StatusPendingDeletion {
		t.Errorf("Status = %v, want PendingDeletion", tn.Status())
	}
	if tn.DeletionReason() != "operator-requested-exit" {
		t.Errorf("Reason = %q", tn.DeletionReason())
	}
	if tn.DeletionScheduledAt().IsZero() {
		t.Error("DeletionScheduledAt is zero")
	}

	events := tn.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	if _, ok := events[0].(tenant.MarkedForDeletionEvent); !ok {
		t.Errorf("event[0] = %T, want MarkedForDeletionEvent", events[0])
	}
}

func TestMarkForDeletion_FromSuspended_Allowed(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newSuspendedTenant(t)
	_ = tn.PullEvents()
	if err := tn.MarkForDeletion("billing-exit"); err != nil {
		t.Fatalf("MarkForDeletion suspended: %v", err)
	}
	if tn.Status() != tenant.StatusPendingDeletion {
		t.Errorf("Status = %v", tn.Status())
	}
}

func TestMarkForDeletion_FromPending_Rejected(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newPendingTenant(t)
	err := tn.MarkForDeletion("never-onboarded")
	if !errors.Is(err, tenant.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestMarkForDeletion_RequiresReason(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newActiveTenant(t)
	for _, raw := range []string{"", "   ", "\t"} {
		if err := tn.MarkForDeletion(raw); !errors.Is(err, tenant.ErrInvalid) {
			t.Errorf("MarkForDeletion(%q): expected ErrInvalid, got %v", raw, err)
		}
	}
}

func TestMarkForDeletion_IdempotentOnSameReason(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newActiveTenant(t)
	_ = tn.MarkForDeletion("exit")
	_ = tn.PullEvents()
	if err := tn.MarkForDeletion("exit"); err != nil {
		t.Errorf("idempotent same reason: %v", err)
	}
	if got := tn.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events on idempotent re-mark, got %d", len(got))
	}
}

func TestMarkForDeletion_RejectedOnDifferentReason(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newActiveTenant(t)
	_ = tn.MarkForDeletion("billing")
	err := tn.MarkForDeletion("compliance")
	if !errors.Is(err, tenant.ErrInvalid) {
		t.Errorf("expected ErrInvalid on conflicting reason, got %v", err)
	}
}

func TestRestoreFromDeletion_FromPendingDeletion_TransitionsToActive(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))

	tn := newActiveTenant(t)
	_ = tn.MarkForDeletion("oops-undo-me")
	_ = tn.PullEvents()

	clock.Set(time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC))
	if err := tn.RestoreFromDeletion(); err != nil {
		t.Fatalf("RestoreFromDeletion: %v", err)
	}
	if tn.Status() != tenant.StatusActive {
		t.Errorf("Status = %v", tn.Status())
	}
	if !tn.DeletionScheduledAt().IsZero() {
		t.Error("DeletionScheduledAt not cleared")
	}
	if tn.DeletionReason() != "" {
		t.Errorf("DeletionReason not cleared: %q", tn.DeletionReason())
	}
	events := tn.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	if _, ok := events[0].(tenant.RestoredEvent); !ok {
		t.Errorf("event[0] = %T, want RestoredEvent", events[0])
	}
}

func TestRestoreFromDeletion_FromActive_NoOp(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newActiveTenant(t)
	_ = tn.PullEvents()
	if err := tn.RestoreFromDeletion(); err != nil {
		t.Errorf("idempotent restore from active: %v", err)
	}
	if got := tn.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events, got %d", len(got))
	}
}

func TestHardDelete_FromPendingDeletion_TransitionsToDeleted(t *testing.T) {
	t.Cleanup(clock.Reset)
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))

	tn := newActiveTenant(t)
	_ = tn.MarkForDeletion("exit")
	_ = tn.PullEvents()

	clock.Set(time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)) // grace expired
	if err := tn.HardDelete(); err != nil {
		t.Fatalf("HardDelete: %v", err)
	}
	if tn.Status() != tenant.StatusDeleted {
		t.Errorf("Status = %v", tn.Status())
	}
	if tn.HardDeletedAt().IsZero() {
		t.Error("HardDeletedAt is zero")
	}

	events := tn.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events: %d", len(events))
	}
	ev, ok := events[0].(tenant.DeletedEvent)
	if !ok {
		t.Fatalf("event[0] = %T, want DeletedEvent", events[0])
	}
	if ev.Reason != "exit" {
		t.Errorf("Reason = %q, want exit", ev.Reason)
	}
}

func TestHardDelete_FromPending_AdminAbandonment(t *testing.T) {
	// Tenant never activated — admin abandonment hard-deletes directly
	// without a grace window since tenant never operated.
	t.Cleanup(clock.Reset)
	tn := newPendingTenant(t)
	_ = tn.PullEvents()
	if err := tn.HardDelete(); err != nil {
		t.Fatalf("HardDelete on pending: %v", err)
	}
	if tn.Status() != tenant.StatusDeleted {
		t.Errorf("Status = %v", tn.Status())
	}
}

func TestHardDelete_FromActive_RejectedWithoutMarking(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newActiveTenant(t)
	if err := tn.HardDelete(); !errors.Is(err, tenant.ErrInvalid) {
		t.Errorf("expected ErrInvalid hard-deleting active tenant, got %v", err)
	}
}

func TestHardDelete_Idempotent(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newPendingTenant(t)
	_ = tn.HardDelete()
	_ = tn.PullEvents()
	if err := tn.HardDelete(); err != nil {
		t.Errorf("idempotent hard-delete: %v", err)
	}
	if got := tn.PullEvents(); len(got) != 0 {
		t.Errorf("expected 0 events on idempotent terminal, got %d", len(got))
	}
}

func TestActivate_RejectedFromPendingDeletion(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newActiveTenant(t)
	_ = tn.MarkForDeletion("exit")
	if err := tn.Activate(); !errors.Is(err, tenant.ErrInvalid) {
		t.Errorf("expected ErrInvalid activating pending-deletion tenant, got %v", err)
	}
}

func TestSuspend_RejectedFromPendingDeletion(t *testing.T) {
	t.Cleanup(clock.Reset)
	tn := newActiveTenant(t)
	_ = tn.MarkForDeletion("exit")
	if err := tn.Suspend("billing"); !errors.Is(err, tenant.ErrInvalid) {
		t.Errorf("expected ErrInvalid suspending pending-deletion tenant, got %v", err)
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
