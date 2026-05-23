// Package product defines the Product aggregate — the Inventory module's
// catalog master per BRD §6.5 + ADR 0061.
//
// Tenant-scoped. Identified by [ID] (UUIDv7). Carries the master fields
// every batch + future order line references: SKU, name, dosage form,
// pack size, HSN code (Indian GST), gst_rate_bps (basis points; never
// float per Stripe canon), manufacturer, is_active flag, soft-delete.
//
// Construction via [New] (factory enforcing invariants) or
// [UnmarshalFromDB] (repository-only re-hydration — does NOT re-validate
// per TDL Wild Workouts canon).
//
// Per the Aggregate-Root rule (Vernon IDDD ch.10): Batches reference
// Products by ID, never by struct embedding. Cross-aggregate consistency
// rides the same-tx outbox in the application layer (the Inventory
// onboarding flow + the Batch.Add handler).
package product

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Length / value bounds (mirrors of the migration's CHECK constraints —
// single source of truth for the invariant per coding-standards.md "No
// magic strings — production AND tests").
const (
	SKUMinLength         = 1
	SKUMaxLength         = 64
	NameMinLength        = 1
	NameMaxLength        = 200
	DosageFormMinLength  = 1
	DosageFormMaxLength  = 50
	PackSizeMinLength    = 1
	PackSizeMaxLength    = 100
	HSNCodeMinLength     = 4
	HSNCodeMaxLength     = 10
	ManufacturerMaxLen   = 200
	GSTRateBpsMin        = 0     // 0%
	GSTRateBpsMax        = 10000 // 100% — practical ceiling; basis points
)

// ErrInvalid is the sentinel returned (wrapped via %w) by [New] +
// [Update] on invariant violation. Callers branch via errors.Is.
var ErrInvalid = errs.New(errs.KindInvalidInput, "product", "invalid product")

// ErrDeleted is returned when a mutation targets a soft-deleted product.
var ErrDeleted = errs.New(errs.KindConflict, "product", "product is deleted")

// ID is the Product primary key — UUIDv7 string. Wrapper type prevents
// accidental swap with other domain IDs (Cheney "type the inputs" canon).
type ID string

// IsZero reports whether ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// Spec is the wire-stable input to [New]. Defensive: invariants checked
// inside New rather than at every call site.
type Spec struct {
	SKU          string
	Name         string
	DosageForm   string
	PackSize     string
	HSNCode      string
	GSTRateBps   int
	Manufacturer string
}

// UpdateSpec is the partial-update payload for [Product.Update]. Only
// non-nil fields are applied; matching values are no-ops (no event).
// Per Vernon IDDD ch.4 "Granular mutators emit granular events" — but
// for the Product master a coarse Update is enough because consumers
// (search index, integration-event subscribers, frontend cache) treat
// "product changed" as a single signal. ChangedFields on the event
// surfaces the diff.
type UpdateSpec struct {
	Name         *string
	GSTRateBps   *int
	IsActive     *bool
	Manufacturer *string
}

// Product is the aggregate root.
//
// Invariants enforced by [New] + [Update]:
//   - id + tenantID non-zero
//   - SKU trimmed, upper-cased, 1..SKUMaxLength
//   - Name trimmed, 1..NameMaxLength
//   - DosageForm + PackSize trimmed, length-bounded
//   - HSNCode 4..10 digits (Indian GST canon)
//   - GSTRateBps in [0, 10000] (basis points; 1200 = 12%)
//   - Manufacturer trimmed, <=ManufacturerMaxLen
type Product struct {
	id           ID
	tenantID     tenant.ID
	sku          string
	name         string
	dosageForm   string
	packSize     string
	hsnCode      string
	gstRateBps   int
	manufacturer string
	isActive     bool
	createdAt    time.Time
	updatedAt    time.Time
	deleted      bool
	deletedAt    time.Time
	deletedBy    string
	events       []Event
}

// New constructs a brand-new Product. Returns ErrInvalid (wrapped) on
// invariant violation. Emits CreatedEvent on success.
//
// actorID is the membership that initiated the create — populates the
// CreatedEvent.ActorID so the integration mapper can stamp
// `created_by_membership_id` on the wire event without re-deriving from
// ctx.
func New(id ID, tenantID tenant.ID, actorID membership.ID, spec Spec) (*Product, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if tenantID.IsZero() {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalid)
	}
	if actorID.IsZero() {
		return nil, fmt.Errorf("%w: actorID required", ErrInvalid)
	}
	sku, err := validateSKU(spec.SKU)
	if err != nil {
		return nil, err
	}
	name, err := validateBoundedTrim(spec.Name, "name", NameMinLength, NameMaxLength)
	if err != nil {
		return nil, err
	}
	dosage, err := validateBoundedTrim(spec.DosageForm, "dosage_form", DosageFormMinLength, DosageFormMaxLength)
	if err != nil {
		return nil, err
	}
	packSize, err := validateBoundedTrim(spec.PackSize, "pack_size", PackSizeMinLength, PackSizeMaxLength)
	if err != nil {
		return nil, err
	}
	hsn, err := validateHSN(spec.HSNCode)
	if err != nil {
		return nil, err
	}
	if spec.GSTRateBps < GSTRateBpsMin || spec.GSTRateBps > GSTRateBpsMax {
		return nil, fmt.Errorf("%w: gst_rate_bps %d not in [%d,%d]",
			ErrInvalid, spec.GSTRateBps, GSTRateBpsMin, GSTRateBpsMax)
	}
	manufacturer := strings.TrimSpace(spec.Manufacturer)
	if len(manufacturer) > ManufacturerMaxLen {
		return nil, fmt.Errorf("%w: manufacturer too long (max %d, got %d)",
			ErrInvalid, ManufacturerMaxLen, len(manufacturer))
	}
	now := clock.Now()
	p := &Product{
		id:           id,
		tenantID:     tenantID,
		sku:          sku,
		name:         name,
		dosageForm:   dosage,
		packSize:     packSize,
		hsnCode:      hsn,
		gstRateBps:   spec.GSTRateBps,
		manufacturer: manufacturer,
		isActive:     true,
		createdAt:    now,
		updatedAt:    now,
	}
	p.recordEvent(CreatedEvent{
		ProductID:    id,
		TenantID:     tenantID,
		ActorID:      actorID,
		SKU:          sku,
		Name:         name,
		DosageForm:   dosage,
		PackSize:     packSize,
		HSNCode:      hsn,
		GSTRateBps:   spec.GSTRateBps,
		Manufacturer: manufacturer,
		At:           now,
	})
	return p, nil
}

// ----- Getters --------------------------------------------------------------

// ID returns the Product's primary key.
func (p *Product) ID() ID { return p.id }

// TenantID returns the FK to [tenant.Tenant].
func (p *Product) TenantID() tenant.ID { return p.tenantID }

// SKU returns the per-tenant stock-keeping unit (always upper-case + trimmed).
func (p *Product) SKU() string { return p.sku }

// Name returns the human-readable product name.
func (p *Product) Name() string { return p.name }

// DosageForm returns the BRD §6.5 dosage form (Tablet, Capsule, Syrup, ...).
func (p *Product) DosageForm() string { return p.dosageForm }

// PackSize returns the pack-size descriptor ("10x10", "100ml bottle", ...).
func (p *Product) PackSize() string { return p.packSize }

// HSNCode returns the Indian Harmonised System of Nomenclature code (4..10 digits).
func (p *Product) HSNCode() string { return p.hsnCode }

// GSTRateBps returns the applicable GST rate in basis points
// (1200 = 12.00%). Never float, never percentage int (Stripe canon
// — money math in integer minor units).
func (p *Product) GSTRateBps() int { return p.gstRateBps }

// Manufacturer returns the declared manufacturer name (may be empty).
func (p *Product) Manufacturer() string { return p.manufacturer }

// IsActive reports whether the product is selectable on order forms +
// product pickers. Independent of soft-delete (deleted ⇒ never visible
// regardless of IsActive).
func (p *Product) IsActive() bool { return p.isActive }

// IsDeleted reports whether the product has been soft-deleted. Live
// read paths filter these; this getter is for forensic tooling.
func (p *Product) IsDeleted() bool { return p.deleted }

// DeletedAt returns the soft-delete timestamp (zero if live).
func (p *Product) DeletedAt() time.Time { return p.deletedAt }

// DeletedBy returns the membership id (or operator id) that performed
// the soft-delete (empty if live).
func (p *Product) DeletedBy() string { return p.deletedBy }

// CreatedAt returns the immutable creation timestamp.
func (p *Product) CreatedAt() time.Time { return p.createdAt }

// UpdatedAt returns the most-recent mutation timestamp.
func (p *Product) UpdatedAt() time.Time { return p.updatedAt }

// ----- State transitions ----------------------------------------------------

// Update applies a partial-update spec. Only non-nil fields are
// considered; matching values are no-ops. Emits UpdatedEvent with
// ChangedFields populated (sorted alphabetically for deterministic
// audit comparison).
//
// actorID is the membership that initiated the change — populates
// UpdatedEvent.ActorID.
//
// Returns ErrDeleted if the Product was soft-deleted.
// Returns ErrInvalid (wrapped) on field-validation failure.
func (p *Product) Update(actorID membership.ID, spec UpdateSpec) error {
	if p.deleted {
		return fmt.Errorf("%w: cannot update deleted product", ErrDeleted)
	}
	if actorID.IsZero() {
		return fmt.Errorf("%w: actorID required", ErrInvalid)
	}
	changed := []string{}
	if spec.Name != nil {
		n, err := validateBoundedTrim(*spec.Name, "name", NameMinLength, NameMaxLength)
		if err != nil {
			return err
		}
		if n != p.name {
			p.name = n
			changed = append(changed, "name")
		}
	}
	if spec.GSTRateBps != nil {
		v := *spec.GSTRateBps
		if v < GSTRateBpsMin || v > GSTRateBpsMax {
			return fmt.Errorf("%w: gst_rate_bps %d not in [%d,%d]",
				ErrInvalid, v, GSTRateBpsMin, GSTRateBpsMax)
		}
		if v != p.gstRateBps {
			p.gstRateBps = v
			changed = append(changed, "gst_rate_bps")
		}
	}
	if spec.IsActive != nil {
		if *spec.IsActive != p.isActive {
			p.isActive = *spec.IsActive
			changed = append(changed, "is_active")
		}
	}
	if spec.Manufacturer != nil {
		m := strings.TrimSpace(*spec.Manufacturer)
		if len(m) > ManufacturerMaxLen {
			return fmt.Errorf("%w: manufacturer too long (max %d, got %d)",
				ErrInvalid, ManufacturerMaxLen, len(m))
		}
		if m != p.manufacturer {
			p.manufacturer = m
			changed = append(changed, "manufacturer")
		}
	}
	if len(changed) == 0 {
		return nil
	}
	slices.Sort(changed)
	now := clock.Now()
	p.updatedAt = now
	p.recordEvent(UpdatedEvent{
		ProductID:     p.id,
		TenantID:      p.tenantID,
		ActorID:       actorID,
		ChangedFields: changed,
		At:            now,
	})
	return nil
}

// Activate sets is_active = true. Convenience wrapper around Update.
func (p *Product) Activate(actorID membership.ID) error {
	t := true
	return p.Update(actorID, UpdateSpec{IsActive: &t})
}

// Deactivate sets is_active = false. Convenience wrapper around Update.
// Distinct from SoftDelete — deactivated products are still visible to
// admins + reports + historical orders; deleted products are not.
func (p *Product) Deactivate(actorID membership.ID) error {
	f := false
	return p.Update(actorID, UpdateSpec{IsActive: &f})
}

// SoftDelete marks the product deleted, recording who did it for audit.
// Idempotent (second call no-ops + emits no event).
//
// CALLER INVARIANT (application layer): SoftDelete must be REJECTED
// when any live Batch with quantity_on_hand > 0 exists for this
// product. The domain refuses cross-aggregate reaches per Vernon
// ch.10; the SoftDeleteProductHandler enforces this rule.
func (p *Product) SoftDelete(actorID membership.ID) error {
	if p.deleted {
		return nil
	}
	if actorID.IsZero() {
		return fmt.Errorf("%w: actorID required for audit", ErrInvalid)
	}
	now := clock.Now()
	p.deleted = true
	p.deletedAt = now
	p.deletedBy = actorID.String()
	p.updatedAt = now
	p.recordEvent(DeactivatedEvent{
		ProductID: p.id,
		TenantID:  p.tenantID,
		ActorID:   actorID,
		At:        now,
	})
	return nil
}

// ----- Persistence DTO ------------------------------------------------------

// Snapshot is the persistence-layer projection consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID           ID
	TenantID     tenant.ID
	SKU          string
	Name         string
	DosageForm   string
	PackSize     string
	HSNCode      string
	GSTRateBps   int
	Manufacturer string
	IsActive     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	IsDeleted    bool
	DeletedAt    time.Time
	DeletedBy    string
}

// UnmarshalFromDB re-hydrates a Product from persistence. Repository-only.
// Does NOT re-validate (TDL canon: trust the DB).
// Does NOT emit events (re-hydration is not a domain transition).
func UnmarshalFromDB(s Snapshot) *Product {
	return &Product{
		id:           s.ID,
		tenantID:     s.TenantID,
		sku:          s.SKU,
		name:         s.Name,
		dosageForm:   s.DosageForm,
		packSize:     s.PackSize,
		hsnCode:      s.HSNCode,
		gstRateBps:   s.GSTRateBps,
		manufacturer: s.Manufacturer,
		isActive:     s.IsActive,
		createdAt:    s.CreatedAt,
		updatedAt:    s.UpdatedAt,
		deleted:      s.IsDeleted,
		deletedAt:    s.DeletedAt,
		deletedBy:    s.DeletedBy,
	}
}

// ----- Event handling -------------------------------------------------------

// PullEvents drains recorded domain events + clears the slice. The
// repository calls this once per persist + writes the resulting events
// to the outbox in the same tx (TDL Sep 2024 UpdateFn pattern).
func (p *Product) PullEvents() []Event {
	if len(p.events) == 0 {
		return nil
	}
	out := p.events
	p.events = nil
	return out
}

func (p *Product) recordEvent(e Event) {
	p.events = append(p.events, e)
}

// ----- Validation helpers ---------------------------------------------------

func validateSKU(raw string) (string, error) {
	trimmed := strings.ToUpper(strings.TrimSpace(raw))
	if trimmed == "" {
		return "", fmt.Errorf("%w: sku required", ErrInvalid)
	}
	if len(trimmed) < SKUMinLength || len(trimmed) > SKUMaxLength {
		return "", fmt.Errorf("%w: sku length %d not in [%d,%d]",
			ErrInvalid, len(trimmed), SKUMinLength, SKUMaxLength)
	}
	return trimmed, nil
}

func validateBoundedTrim(raw, field string, min, max int) (string, error) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", fmt.Errorf("%w: %s required", ErrInvalid, field)
	}
	if len(t) < min || len(t) > max {
		return "", fmt.Errorf("%w: %s length %d not in [%d,%d]",
			ErrInvalid, field, len(t), min, max)
	}
	return t, nil
}

func validateHSN(raw string) (string, error) {
	t := strings.TrimSpace(raw)
	if len(t) < HSNCodeMinLength || len(t) > HSNCodeMaxLength {
		return "", fmt.Errorf("%w: hsn_code length %d not in [%d,%d]",
			ErrInvalid, len(t), HSNCodeMinLength, HSNCodeMaxLength)
	}
	for _, r := range t {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("%w: hsn_code must be all-digits (got %q)", ErrInvalid, t)
		}
	}
	return t, nil
}
