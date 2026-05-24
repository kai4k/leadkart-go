package product_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// fixedNow is the deterministic timestamp every product domain test
// passes to factories + mutators per the clock-injection refactor.
var fixedNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// freshIDs is a tiny helper — random Product ID, Tenant ID, Actor ID per test.
func freshIDs(t *testing.T) (product.ID, tenant.ID, membership.ID) {
	t.Helper()
	return product.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		membership.ID(ids.NewV7().String())
}

// validSpec returns a baseline-valid Spec used by every test that doesn't
// care about input validation. Centralising avoids per-test drift when
// invariants tighten.
func validSpec() product.Spec {
	return product.Spec{
		SKU:          "AMOX-500",
		Name:         "Amoxicillin 500 mg",
		DosageForm:   "Capsule",
		PackSize:     "10x10",
		HSNCode:      "30049099",
		GSTRateBps:   1200, // 12%
		Manufacturer: "Acme Pharma",
	}
}

func freshProduct(t *testing.T) *product.Product {
	t.Helper()
	pid, tid, actor := freshIDs(t)
	p, err := product.New(pid, tid, actor, validSpec(), fixedNow)
	if err != nil {
		t.Fatalf("product.New: %v", err)
	}
	// Drain creation event so per-test expectations target NEW events.
	_ = p.PullEvents()
	return p
}

func TestNew_HappyPath_SetsFieldsAndEmitsCreated(t *testing.T) {
	t.Parallel()
	pid, tid, actor := freshIDs(t)
	p, err := product.New(pid, tid, actor, validSpec(), fixedNow)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.ID() != pid || p.TenantID() != tid {
		t.Fatalf("ids: got (%q,%q) want (%q,%q)", p.ID(), p.TenantID(), pid, tid)
	}
	if p.SKU() != "AMOX-500" || p.Name() != "Amoxicillin 500 mg" {
		t.Fatalf("sku/name: %q %q", p.SKU(), p.Name())
	}
	if p.GSTRateBps() != 1200 || p.HSNCode() != "30049099" {
		t.Fatalf("hsn/gst: %q %d", p.HSNCode(), p.GSTRateBps())
	}
	if !p.IsActive() {
		t.Fatal("IsActive: want true on construction")
	}
	if p.IsDeleted() {
		t.Fatal("IsDeleted: want false on construction")
	}
	if p.CreatedAt().IsZero() {
		t.Fatal("CreatedAt zero")
	}
	if p.UpdatedAt() != p.CreatedAt() {
		t.Fatal("UpdatedAt should equal CreatedAt on construction")
	}
	evs := p.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: got %d want 1", len(evs))
	}
	created, ok := evs[0].(product.CreatedEvent)
	if !ok {
		t.Fatalf("event type: got %T", evs[0])
	}
	if created.ActorID != actor {
		t.Fatalf("actor: got %q want %q", created.ActorID, actor)
	}
}

func TestNew_TrimsAndNormalizesSKU(t *testing.T) {
	t.Parallel()
	pid, tid, actor := freshIDs(t)
	spec := validSpec()
	spec.SKU = "  amox-500  "
	spec.Name = " Amoxicillin "
	p, err := product.New(pid, tid, actor, spec, fixedNow)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// SKU upper-cased + trimmed for case-insensitive uniqueness behaviour.
	if p.SKU() != "AMOX-500" {
		t.Fatalf("SKU normalisation: got %q want AMOX-500", p.SKU())
	}
	if p.Name() != "Amoxicillin" {
		t.Fatalf("Name trim: %q", p.Name())
	}
}

func TestNew_InvalidInputs(t *testing.T) {
	t.Parallel()
	pid, tid, actor := freshIDs(t)
	cases := []struct {
		name  string
		mut   func(s *product.Spec)
		zeroP bool
		zeroT bool
		zeroA bool
	}{
		{name: "zero id", zeroP: true},
		{name: "zero tenant", zeroT: true},
		{name: "zero actor", zeroA: true},
		{name: "empty sku", mut: func(s *product.Spec) { s.SKU = "" }},
		{name: "sku too long", mut: func(s *product.Spec) { s.SKU = strings.Repeat("a", 65) }},
		{name: "empty name", mut: func(s *product.Spec) { s.Name = "" }},
		{name: "name too long", mut: func(s *product.Spec) { s.Name = strings.Repeat("a", 201) }},
		{name: "empty dosage form", mut: func(s *product.Spec) { s.DosageForm = "" }},
		{name: "empty pack size", mut: func(s *product.Spec) { s.PackSize = "" }},
		{name: "hsn too short", mut: func(s *product.Spec) { s.HSNCode = "300" }},
		{name: "hsn too long", mut: func(s *product.Spec) { s.HSNCode = "30049099XX" }},
		{name: "hsn non-numeric", mut: func(s *product.Spec) { s.HSNCode = "3004XYZX" }},
		{name: "gst negative", mut: func(s *product.Spec) { s.GSTRateBps = -1 }},
		{name: "gst too high", mut: func(s *product.Spec) { s.GSTRateBps = 10001 }},
		{name: "manufacturer too long", mut: func(s *product.Spec) { s.Manufacturer = strings.Repeat("a", 201) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := validSpec()
			if tc.mut != nil {
				tc.mut(&spec)
			}
			id := pid
			tid := tid
			a := actor
			if tc.zeroP {
				id = product.ID("")
			}
			if tc.zeroT {
				tid = tenant.ID("")
			}
			if tc.zeroA {
				a = membership.ID("")
			}
			if _, err := product.New(id, tid, a, spec, fixedNow); !errors.Is(err, product.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestUpdate_PartialUpdate_EmitsUpdatedEventWithChangedFields(t *testing.T) {
	t.Parallel()
	p := freshProduct(t)
	createdAt := p.CreatedAt()
	actor := membership.ID(ids.NewV7().String())
	if err := p.Update(actor, product.UpdateSpec{
		Name:       strPtr("Amoxicillin 500 mg Capsules"),
		GSTRateBps: intPtr(1800),
	}, fixedNow.Add(time.Second)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if p.Name() != "Amoxicillin 500 mg Capsules" {
		t.Fatalf("Name not updated: %q", p.Name())
	}
	if p.GSTRateBps() != 1800 {
		t.Fatalf("GST not updated: %d", p.GSTRateBps())
	}
	if p.UpdatedAt().Before(createdAt) {
		t.Fatalf("UpdatedAt regressed: %v vs %v", p.UpdatedAt(), createdAt)
	}
	evs := p.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: got %d want 1", len(evs))
	}
	upd, ok := evs[0].(product.UpdatedEvent)
	if !ok {
		t.Fatalf("type: %T", evs[0])
	}
	if upd.ActorID != actor {
		t.Fatalf("actor: got %q want %q", upd.ActorID, actor)
	}
	if len(upd.ChangedFields) != 2 {
		t.Fatalf("changed fields: %v", upd.ChangedFields)
	}
	gotChanged := map[string]bool{}
	for _, f := range upd.ChangedFields {
		gotChanged[f] = true
	}
	if !gotChanged["name"] || !gotChanged["gst_rate_bps"] {
		t.Fatalf("changed fields not equal: %v", upd.ChangedFields)
	}
}

func TestUpdate_NoOp_NoEvent(t *testing.T) {
	t.Parallel()
	p := freshProduct(t)
	actor := membership.ID(ids.NewV7().String())
	// Update with all fields nil = no-op.
	if err := p.Update(actor, product.UpdateSpec{}, fixedNow); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(p.PullEvents()) != 0 {
		t.Fatal("no-op update should emit no event")
	}
}

func TestUpdate_SameValue_NoEvent(t *testing.T) {
	t.Parallel()
	p := freshProduct(t)
	actor := membership.ID(ids.NewV7().String())
	same := p.Name()
	if err := p.Update(actor, product.UpdateSpec{Name: &same}, fixedNow); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(p.PullEvents()) != 0 {
		t.Fatal("same-value update should emit no event")
	}
}

func TestUpdate_RejectsInvalidField(t *testing.T) {
	t.Parallel()
	p := freshProduct(t)
	actor := membership.ID(ids.NewV7().String())
	bad := ""
	if err := p.Update(actor, product.UpdateSpec{Name: &bad}, fixedNow); !errors.Is(err, product.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestUpdate_RejectsZeroActor(t *testing.T) {
	t.Parallel()
	p := freshProduct(t)
	n := "X"
	if err := p.Update(membership.ID(""), product.UpdateSpec{Name: &n}, fixedNow); !errors.Is(err, product.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestUpdate_RejectsAfterSoftDelete(t *testing.T) {
	t.Parallel()
	p := freshProduct(t)
	actor := membership.ID(ids.NewV7().String())
	if err := p.SoftDelete(actor, fixedNow); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	_ = p.PullEvents()
	n := "X"
	if err := p.Update(actor, product.UpdateSpec{Name: &n}, fixedNow); !errors.Is(err, product.ErrDeleted) {
		t.Fatalf("want ErrDeleted, got %v", err)
	}
}

// TestSoftDelete_EmitsSoftDeletedEventOnce — per ADR 0061 amendment 1,
// SoftDelete emits a dedicated SoftDeletedEvent (not the older
// DeactivatedEvent — the two were semantically conflated). Idempotent:
// second call no-ops + emits nothing.
func TestSoftDelete_EmitsSoftDeletedEventOnce(t *testing.T) {
	t.Parallel()
	p := freshProduct(t)
	actor := membership.ID(ids.NewV7().String())
	if err := p.SoftDelete(actor, fixedNow); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if !p.IsDeleted() {
		t.Fatal("IsDeleted should be true after SoftDelete")
	}
	if p.DeletedBy() != actor.String() {
		t.Fatalf("DeletedBy: got %q want %q", p.DeletedBy(), actor.String())
	}
	evs := p.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events first: %d", len(evs))
	}
	softDel, ok := evs[0].(product.SoftDeletedEvent)
	if !ok {
		t.Fatalf("type: %T (want SoftDeletedEvent)", evs[0])
	}
	if softDel.ActorID != actor {
		t.Fatalf("actor: got %q want %q", softDel.ActorID, actor)
	}
	// Second call is idempotent — no event.
	if err := p.SoftDelete(actor, fixedNow); err != nil {
		t.Fatalf("second SoftDelete: %v", err)
	}
	if len(p.PullEvents()) != 0 {
		t.Fatal("second SoftDelete should be no-op")
	}
}

// TestDeactivate_EmitsBothUpdatedAndDeactivatedEvents — per ADR 0061
// amendment 1: Deactivate transitions is_active=false AND emits a
// dedicated DeactivatedEvent in addition to the UpdatedEvent. Consumers
// can route either on the diff (UpdatedEvent.ChangedFields contains
// "is_active") or on the lifecycle signal (DeactivatedEvent). Distinct
// from SoftDelete (terminal hide).
func TestDeactivate_EmitsBothUpdatedAndDeactivatedEvents(t *testing.T) {
	t.Parallel()
	p := freshProduct(t)
	actor := membership.ID(ids.NewV7().String())
	if err := p.Deactivate(actor, fixedNow); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if p.IsActive() {
		t.Fatal("IsActive should be false after Deactivate")
	}
	if p.IsDeleted() {
		t.Fatal("IsDeleted should be false (Deactivate != SoftDelete)")
	}
	evs := p.PullEvents()
	if len(evs) != 2 {
		t.Fatalf("event count: got %d want 2 (Updated + Deactivated)", len(evs))
	}
	if _, ok := evs[0].(product.UpdatedEvent); !ok {
		t.Fatalf("first event: %T (want UpdatedEvent)", evs[0])
	}
	if _, ok := evs[1].(product.DeactivatedEvent); !ok {
		t.Fatalf("second event: %T (want DeactivatedEvent)", evs[1])
	}
	// Second Deactivate on an already-inactive product is a no-op.
	if err := p.Deactivate(actor, fixedNow); err != nil {
		t.Fatalf("second Deactivate: %v", err)
	}
	if len(p.PullEvents()) != 0 {
		t.Fatal("second Deactivate on inactive product should be no-op")
	}
}

func TestActivate_Deactivate_TogglesAndEmitsEvent(t *testing.T) {
	t.Parallel()
	p := freshProduct(t)
	actor := membership.ID(ids.NewV7().String())
	if err := p.Deactivate(actor, fixedNow); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if p.IsActive() {
		t.Fatal("IsActive should be false")
	}
	evs := p.PullEvents()
	// Deactivate emits BOTH UpdatedEvent (with ChangedFields=["is_active"])
	// AND a dedicated DeactivatedEvent per ADR 0061 amendment 1.
	if len(evs) != 2 {
		t.Fatalf("events: got %d want 2 (UpdatedEvent + DeactivatedEvent)", len(evs))
	}
	upd, ok := evs[0].(product.UpdatedEvent)
	if !ok || len(upd.ChangedFields) != 1 || upd.ChangedFields[0] != "is_active" {
		t.Fatalf("type/changed: %T %v", evs[0], evs[0])
	}
	if _, ok := evs[1].(product.DeactivatedEvent); !ok {
		t.Fatalf("second event: %T (want DeactivatedEvent)", evs[1])
	}
	if err := p.Activate(actor, fixedNow); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !p.IsActive() {
		t.Fatal("Re-activation failed")
	}
	if len(p.PullEvents()) != 1 {
		t.Fatal("Activate should emit UpdatedEvent(is_active)")
	}
}

func TestUnmarshalFromDB_DoesNotEmitEvents(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(ids.NewV7().String())
	pid := product.ID(ids.NewV7().String())
	p := product.UnmarshalFromDB(product.Snapshot{
		ID:       pid,
		TenantID: tid,
		SKU:      "X-1",
		Name:     "Stored Product",
		IsActive: true,
	})
	if p.ID() != pid {
		t.Fatal("id round-trip")
	}
	if len(p.PullEvents()) != 0 {
		t.Fatal("UnmarshalFromDB must not emit events")
	}
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }
