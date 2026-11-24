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

// fixedNow is the deterministic clock value passed to all product factories and mutators.
var fixedNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// freshIDs returns a fresh random Product ID, Tenant ID, and Actor ID.
func freshIDs(t *testing.T) (product.ID, tenant.ID, membership.ID) {
	t.Helper()
	return product.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		membership.ID(ids.NewV7().String())
}

// validSpec returns a baseline-valid Spec for tests that do not exercise validation.
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
	// Drain the creation event so test assertions target only subsequent events.
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
	// SKU is upper-cased and trimmed for case-insensitive uniqueness.
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
	// All-nil spec = no-op.
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

// ADR 0073: a product goes inactive and active again, never deleted. Each transition emits its
// own event once; repeating it is a no-op with no event.
func TestDeactivate_EmitsDeactivatedEventOnce(t *testing.T) {
	t.Parallel()
	p := freshProduct(t)
	actor := membership.ID(ids.NewV7().String())
	if err := p.Deactivate(actor, fixedNow); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if p.IsActive() {
		t.Fatal("IsActive should be false after Deactivate")
	}
	evs := p.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("event count: got %d want 1", len(evs))
	}
	if ev, ok := evs[0].(product.DeactivatedEvent); !ok || ev.ActorID != actor {
		t.Fatalf("event: %T %+v (want DeactivatedEvent by the actor)", evs[0], evs[0])
	}
	if err := p.Deactivate(actor, fixedNow); err != nil {
		t.Fatalf("second Deactivate: %v", err)
	}
	if len(p.PullEvents()) != 0 {
		t.Fatal("second Deactivate on an inactive product should be a no-op")
	}
}

func TestActivate_EmitsActivatedEventOnceAndIsANoOpWhenActive(t *testing.T) {
	t.Parallel()
	p := freshProduct(t)
	actor := membership.ID(ids.NewV7().String())
	if err := p.Activate(actor, fixedNow); err != nil {
		t.Fatalf("Activate on an active product: %v", err)
	}
	if len(p.PullEvents()) != 0 {
		t.Fatal("Activate on an active product should be a no-op")
	}
	if err := p.Deactivate(actor, fixedNow); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	_ = p.PullEvents()
	if err := p.Activate(actor, fixedNow); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !p.IsActive() {
		t.Fatal("re-activation failed")
	}
	evs := p.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: got %d want 1", len(evs))
	}
	if _, ok := evs[0].(product.ActivatedEvent); !ok {
		t.Fatalf("event: %T (want ActivatedEvent)", evs[0])
	}
}

func TestDeactivateAndActivate_RequireAnActor(t *testing.T) {
	t.Parallel()
	p := freshProduct(t)
	if err := p.Deactivate(membership.ID(""), fixedNow); !errors.Is(err, product.ErrInvalid) {
		t.Fatalf("Deactivate without actor: %v, want ErrInvalid", err)
	}
	if err := p.Activate(membership.ID(""), fixedNow); !errors.Is(err, product.ErrInvalid) {
		t.Fatalf("Activate without actor: %v, want ErrInvalid", err)
	}
}

// An inactive product is still readable and editable: only new batches are refused (at the
// handler), the aggregate itself keeps working.
func TestUpdate_StillWorksOnAnInactiveProduct(t *testing.T) {
	t.Parallel()
	p := freshProduct(t)
	actor := membership.ID(ids.NewV7().String())
	if err := p.Deactivate(actor, fixedNow); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	n := "Renamed while inactive"
	if err := p.Update(actor, product.UpdateSpec{Name: &n}, fixedNow); err != nil {
		t.Fatalf("Update on an inactive product: %v", err)
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

// I7: the Product domain uses the one HSN rule (common/hsncode): trimmed, ASCII
// digits only, 4, 6 or 8 of them, leading zero allowed.
func TestNew_HSNRule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		hsn  string
		want string // "" means refused
	}{
		{"3004", "3004"}, {"300410", "300410"}, {"30041020", "30041020"},
		{"0402", "0402"}, {"04021010", "04021010"}, // chapters 01-09 are real
		{" 3004 ", "3004"},                                                    // trimmed
		{"30041", ""}, {"3004102", ""}, {"300410200", ""}, {"3004102000", ""}, // 5, 7, 9, 10 digits passed before
		{"300", ""}, {"30", ""}, {"3004XYZX", ""}, {"30 04", ""},
	}
	for _, tc := range cases {
		t.Run(tc.hsn, func(t *testing.T) {
			t.Parallel()
			pid, tid, actor := freshIDs(t)
			spec := validSpec()
			spec.HSNCode = tc.hsn
			p, err := product.New(pid, tid, actor, spec, fixedNow)
			if tc.want == "" {
				if !errors.Is(err, product.ErrInvalid) {
					t.Fatalf("HSN %q: want ErrInvalid, got %v", tc.hsn, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("HSN %q: %v", tc.hsn, err)
			}
			if p.HSNCode() != tc.want {
				t.Fatalf("HSN %q stored as %q, want %q", tc.hsn, p.HSNCode(), tc.want)
			}
		})
	}
}

// The reorder level and the expiry-alert threshold have a ceiling: both are
// int columns, and the threshold is added to a date by the expiry scan. A
// value above the ceiling used to pass the aggregate and fail (or wrap) at the
// column.
func TestNewAndUpdate_ReorderLevelAndThresholdCeilings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		reorder   int
		threshold int
		wantErr   bool
	}{
		{"at the ceilings", 2147483647, 36500, false},
		{"reorder level over the column", 2147483648, 90, true},
		{"threshold over 100 years", 0, 36501, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			pid, tid, actor := freshIDs(t)
			spec := validSpec()
			spec.ReorderLevel, spec.ExpiryAlertThresholdDays = c.reorder, c.threshold
			_, err := product.New(pid, tid, actor, spec, fixedNow)
			if got := errors.Is(err, product.ErrInvalid); got != c.wantErr {
				t.Fatalf("New: err %v, want ErrInvalid=%v", err, c.wantErr)
			}

			p := freshProduct(t)
			err = p.Update(actor, product.UpdateSpec{
				ReorderLevel: intPtr(c.reorder), ExpiryAlertThresholdDays: intPtr(c.threshold),
			}, fixedNow)
			if got := errors.Is(err, product.ErrInvalid); got != c.wantErr {
				t.Fatalf("Update: err %v, want ErrInvalid=%v", err, c.wantErr)
			}
			if c.wantErr && (p.ReorderLevel() != 0 || p.ExpiryAlertThresholdDays() != 0) {
				t.Fatalf("a refused update changed the product: reorder %d threshold %d", p.ReorderLevel(), p.ExpiryAlertThresholdDays())
			}
		})
	}
}
