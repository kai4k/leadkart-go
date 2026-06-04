// Package product defines the Product aggregate — Inventory catalog master per BRD §6.5 + ADR 0061.
//
// Tenant-scoped; identified by [ID] (UUIDv7). Carries the fields every Batch and order line
// references: SKU, name, dosage form, pack size, HSN code, gst_rate_bps (basis points, never float
// per Stripe canon), manufacturer, is_active, soft-delete.
//
// Construct via [New] (invariants enforced) or [UnmarshalFromDB] (repo re-hydration, no re-validation
// per TDL Wild Workouts canon). Batches reference Products by ID only (Vernon IDDD ch.10);
// cross-aggregate consistency via same-tx outbox in the application layer.
package product

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Length / value bounds — mirrors the migration's CHECK constraints (single source of truth).
const (
	SKUMinLength        = 1
	SKUMaxLength        = 64
	NameMinLength       = 1
	NameMaxLength       = 200
	DosageFormMinLength = 1
	DosageFormMaxLength = 50
	PackSizeMinLength   = 1
	PackSizeMaxLength   = 100
	HSNCodeMinLength    = 4
	HSNCodeMaxLength    = 10
	ManufacturerMaxLen  = 200
	GSTRateBpsMin       = 0     // 0%
	GSTRateBpsMax       = 10000 // 100% — practical ceiling; basis points
	// ReorderLevelMin floors the reorder_level field. 0 disables the
	// per-product reorder alert (the ReorderScanJob skips zero rows).
	ReorderLevelMin = 0
	// ExpiryAlertThresholdDaysMin / Default — per BRD §6.5.
	ExpiryAlertThresholdDaysMin     = 0
	ExpiryAlertThresholdDaysDefault = 90
	// ProductCategory bounds — mirror the migration's CHECK + DEFAULT.
	ProductCategoryMinLength = 1
	ProductCategoryMaxLength = 64
	ProductCategoryDefault   = "General"
)

// ErrInvalid is returned (wrapped via %w) by [New] and [Update] on invariant violation.
var ErrInvalid = errs.New(errs.KindInvalidInput, "product", "invalid product")

// ErrDeleted is returned when a mutating method targets a soft-deleted product.
var ErrDeleted = errs.New(errs.KindConflict, "product", "product is deleted")

// ID is the Product primary key — UUIDv7 string. Distinct type prevents accidental swap with other domain IDs.
type ID string

// IsZero reports whether ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// Spec is the input to [New]. Invariants are validated inside New.
type Spec struct {
	SKU                      string
	Name                     string
	DosageForm               string
	PackSize                 string
	HSNCode                  string
	GSTRateBps               int
	Manufacturer             string
	ReorderLevel             int
	ExpiryAlertThresholdDays int
	ProductCategory          string
}

// UpdateSpec is the partial-update input for [Product.Update].
// Only non-nil fields are applied; matching values are no-ops (no event emitted).
// ChangedFields on the resulting event surfaces the diff.
type UpdateSpec struct {
	Name                     *string
	GSTRateBps               *int
	IsActive                 *bool
	Manufacturer             *string
	ReorderLevel             *int
	ExpiryAlertThresholdDays *int
	ProductCategory          *string
}

// Product is the aggregate root.
//
// Invariants enforced by [New] and [Update]:
//   - id + tenantID non-zero
//   - SKU trimmed + upper-cased, 1..SKUMaxLength
//   - Name trimmed, 1..NameMaxLength
//   - DosageForm + PackSize trimmed, length-bounded
//   - HSNCode 4..10 digits (Indian GST)
//   - GSTRateBps in [0, 10000] (basis points; 1200 = 12%)
//   - Manufacturer trimmed, <=ManufacturerMaxLen
type Product struct {
	id                       ID
	tenantID                 tenant.ID
	sku                      string
	name                     string
	dosageForm               string
	packSize                 string
	hsnCode                  string
	gstRateBps               int
	manufacturer             string
	isActive                 bool
	reorderLevel             int
	expiryAlertThresholdDays int
	productCategory          string
	createdAt                time.Time
	updatedAt                time.Time
	deleted                  bool
	deletedAt                time.Time
	deletedBy                string
	events                   []Event
}

// validateUUID enforces the H6 reviewer rule: every domain ID must parse as a
// UUID at AGGREGATE-CONSTRUCTION time, not later at the adapter boundary.
func validateUUID(name, val string) error {
	val = strings.TrimSpace(val)
	if val == "" {
		return fmt.Errorf("%w: %s required", ErrInvalid, name)
	}
	if _, err := uuid.Parse(val); err != nil {
		return fmt.Errorf("%w: %s not a valid uuid", ErrInvalid, name)
	}
	return nil
}

// New constructs a Product, validates all invariants, and emits [CreatedEvent].
// Returns ErrInvalid (wrapped) on violation.
// actorID populates CreatedEvent.ActorID for the integration mapper.
// now is the explicit clock instant; the aggregate has no temporal dependency.
func New(id ID, tenantID tenant.ID, actorID membership.ID, spec Spec, now time.Time) (*Product, error) {
	if err := validateUUID("id", id.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("tenantID", tenantID.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("actorID", actorID.String()); err != nil {
		return nil, err
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
	if spec.ReorderLevel < ReorderLevelMin {
		return nil, fmt.Errorf("%w: reorder_level must be >= %d (got %d)",
			ErrInvalid, ReorderLevelMin, spec.ReorderLevel)
	}
	if spec.ExpiryAlertThresholdDays < ExpiryAlertThresholdDaysMin {
		return nil, fmt.Errorf("%w: expiry_alert_threshold_days must be >= %d (got %d)",
			ErrInvalid, ExpiryAlertThresholdDaysMin, spec.ExpiryAlertThresholdDays)
	}
	category := strings.TrimSpace(spec.ProductCategory)
	if category == "" {
		category = ProductCategoryDefault
	}
	if len(category) < ProductCategoryMinLength || len(category) > ProductCategoryMaxLength {
		return nil, fmt.Errorf("%w: product_category length %d not in [%d,%d]",
			ErrInvalid, len(category), ProductCategoryMinLength, ProductCategoryMaxLength)
	}
	now = now.UTC()
	p := &Product{
		id:                       id,
		tenantID:                 tenantID,
		sku:                      sku,
		name:                     name,
		dosageForm:               dosage,
		packSize:                 packSize,
		hsnCode:                  hsn,
		gstRateBps:               spec.GSTRateBps,
		manufacturer:             manufacturer,
		isActive:                 true,
		reorderLevel:             spec.ReorderLevel,
		expiryAlertThresholdDays: spec.ExpiryAlertThresholdDays,
		productCategory:          category,
		createdAt:                now,
		updatedAt:                now,
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

// ID returns the Product's primary key.
func (p *Product) ID() ID { return p.id }

// TenantID returns the FK to [tenant.Tenant].
func (p *Product) TenantID() tenant.ID { return p.tenantID }

// SKU returns the per-tenant stock-keeping unit (upper-case, trimmed).
func (p *Product) SKU() string { return p.sku }

// Name returns the human-readable product name.
func (p *Product) Name() string { return p.name }

// DosageForm returns the dosage form (Tablet, Capsule, Syrup, ...).
func (p *Product) DosageForm() string { return p.dosageForm }

// PackSize returns the pack-size descriptor ("10x10", "100ml bottle", ...).
func (p *Product) PackSize() string { return p.packSize }

// HSNCode returns the Indian HSN code (4..10 digits).
func (p *Product) HSNCode() string { return p.hsnCode }

// GSTRateBps returns the GST rate in basis points (1200 = 12.00%; never float).
func (p *Product) GSTRateBps() int { return p.gstRateBps }

// Manufacturer returns the declared manufacturer name (may be empty).
func (p *Product) Manufacturer() string { return p.manufacturer }

// IsActive reports whether the product is selectable on order forms.
// A soft-deleted product is never visible regardless of this flag.
func (p *Product) IsActive() bool { return p.isActive }

// ReorderLevel returns the per-product reorder threshold (units). When
// total live-batch quantity_on_hand falls strictly below this value the
// daily ReorderScanJob emits ProductBelowReorderLevelV1. 0 disables.
func (p *Product) ReorderLevel() int { return p.reorderLevel }

// ExpiryAlertThresholdDays returns the look-ahead window (days) the
// ExpiryScanJob uses to detect batches nearing expiry. BRD default 90.
func (p *Product) ExpiryAlertThresholdDays() int { return p.expiryAlertThresholdDays }

// ProductCategory returns the product's BRD §6.5 category. Drives the
// default GST percentage (resolved via shared.product_category_gst_defaults)
// and matches lead ProductRanges for catalogue browsing.
func (p *Product) ProductCategory() string { return p.productCategory }

// IsDeleted reports whether the product has been soft-deleted. Live
// read paths filter these; this getter is for forensic tooling.
func (p *Product) IsDeleted() bool { return p.deleted }

// DeletedAt returns the soft-delete timestamp; zero if the product is live.
func (p *Product) DeletedAt() time.Time { return p.deletedAt }

// DeletedBy returns the membership or operator ID that performed the soft-delete; empty if live.
func (p *Product) DeletedBy() string { return p.deletedBy }

// CreatedAt returns the creation timestamp (immutable).
func (p *Product) CreatedAt() time.Time { return p.createdAt }

// UpdatedAt returns the most recent mutation timestamp.
func (p *Product) UpdatedAt() time.Time { return p.updatedAt }

// Update applies a partial-update spec; only non-nil fields are applied.
// Matching values are no-ops (no event emitted).
// Emits [UpdatedEvent] with ChangedFields sorted alphabetically.
// Returns [ErrDeleted] if soft-deleted, [ErrInvalid] on validation failure.
func (p *Product) Update(actorID membership.ID, spec UpdateSpec, now time.Time) error {
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
	ext, err := p.applyExtendedUpdates(spec)
	if err != nil {
		return err
	}
	changed = append(changed, ext...)
	if len(changed) == 0 {
		return nil
	}
	slices.Sort(changed)
	now = now.UTC()
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

// applyExtendedUpdates applies the Phase A.3 optional fields (reorder level,
// expiry-alert threshold, product category), returning the changed-field names.
// Split out of [Update] to keep that method under the cyclomatic-complexity gate.
func (p *Product) applyExtendedUpdates(spec UpdateSpec) ([]string, error) {
	changed := []string{}
	if spec.ReorderLevel != nil {
		v := *spec.ReorderLevel
		if v < ReorderLevelMin {
			return nil, fmt.Errorf("%w: reorder_level must be >= %d (got %d)",
				ErrInvalid, ReorderLevelMin, v)
		}
		if v != p.reorderLevel {
			p.reorderLevel = v
			changed = append(changed, "reorder_level")
		}
	}
	if spec.ExpiryAlertThresholdDays != nil {
		v := *spec.ExpiryAlertThresholdDays
		if v < ExpiryAlertThresholdDaysMin {
			return nil, fmt.Errorf("%w: expiry_alert_threshold_days must be >= %d (got %d)",
				ErrInvalid, ExpiryAlertThresholdDaysMin, v)
		}
		if v != p.expiryAlertThresholdDays {
			p.expiryAlertThresholdDays = v
			changed = append(changed, "expiry_alert_threshold_days")
		}
	}
	if spec.ProductCategory != nil {
		c := strings.TrimSpace(*spec.ProductCategory)
		if c == "" {
			c = ProductCategoryDefault
		}
		if len(c) < ProductCategoryMinLength || len(c) > ProductCategoryMaxLength {
			return nil, fmt.Errorf("%w: product_category length %d not in [%d,%d]",
				ErrInvalid, len(c), ProductCategoryMinLength, ProductCategoryMaxLength)
		}
		if c != p.productCategory {
			p.productCategory = c
			changed = append(changed, "product_category")
		}
	}
	return changed, nil
}

// Activate sets is_active = true via [Update].
func (p *Product) Activate(actorID membership.ID, now time.Time) error {
	tr := true
	return p.Update(actorID, UpdateSpec{IsActive: &tr}, now)
}

// Deactivate sets is_active = false via [Update] and additionally emits [DeactivatedEvent]
// so consumers can route on the lifecycle signal without inspecting ChangedFields.
// Distinct from [SoftDelete] — deactivated products remain visible to admins (ADR 0061 amendment 1).
// Idempotent: already-inactive products are a no-op.
func (p *Product) Deactivate(actorID membership.ID, now time.Time) error {
	if !p.isActive {
		return nil // already inactive
	}
	if err := p.Update(actorID, UpdateSpec{IsActive: new(false)}, now); err != nil {
		return err
	}
	// Update already emitted UpdatedEvent(ChangedFields=["is_active"]);
	// emit DeactivatedEvent for consumers routing on the lifecycle signal.
	p.recordEvent(DeactivatedEvent{
		ProductID: p.id,
		TenantID:  p.tenantID,
		ActorID:   actorID,
		At:        p.updatedAt,
	})
	return nil
}

// SoftDelete marks the product deleted for audit. Idempotent — second call is a no-op.
// Caller invariant (application layer): reject when any live Batch has quantity_on_hand > 0;
// the domain cannot cross aggregate boundaries (Vernon ch.10), so SoftDeleteProductHandler enforces this.
func (p *Product) SoftDelete(actorID membership.ID, now time.Time) error {
	if p.deleted {
		return nil
	}
	if actorID.IsZero() {
		return fmt.Errorf("%w: actorID required for audit", ErrInvalid)
	}
	now = now.UTC()
	p.deleted = true
	p.deletedAt = now
	p.deletedBy = actorID.String()
	p.updatedAt = now
	p.recordEvent(SoftDeletedEvent{
		ProductID: p.id,
		TenantID:  p.tenantID,
		ActorID:   actorID,
		At:        now,
	})
	return nil
}

// Snapshot is the persistence-layer projection consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID                       ID
	TenantID                 tenant.ID
	SKU                      string
	Name                     string
	DosageForm               string
	PackSize                 string
	HSNCode                  string
	GSTRateBps               int
	Manufacturer             string
	IsActive                 bool
	ReorderLevel             int
	ExpiryAlertThresholdDays int
	ProductCategory          string
	CreatedAt                time.Time
	UpdatedAt                time.Time
	IsDeleted                bool
	DeletedAt                time.Time
	DeletedBy                string
}

// UnmarshalFromDB re-hydrates a Product from persistence.
// Repository-only — does not re-validate (trust the DB) and does not emit events.
func UnmarshalFromDB(s Snapshot) *Product {
	cat := s.ProductCategory
	if cat == "" {
		cat = ProductCategoryDefault
	}
	return &Product{
		id:                       s.ID,
		tenantID:                 s.TenantID,
		sku:                      s.SKU,
		name:                     s.Name,
		dosageForm:               s.DosageForm,
		packSize:                 s.PackSize,
		hsnCode:                  s.HSNCode,
		gstRateBps:               s.GSTRateBps,
		manufacturer:             s.Manufacturer,
		isActive:                 s.IsActive,
		reorderLevel:             s.ReorderLevel,
		expiryAlertThresholdDays: s.ExpiryAlertThresholdDays,
		productCategory:          cat,
		createdAt:                s.CreatedAt,
		updatedAt:                s.UpdatedAt,
		deleted:                  s.IsDeleted,
		deletedAt:                s.DeletedAt,
		deletedBy:                s.DeletedBy,
	}
}

// PullEvents drains and returns recorded domain events, then clears the slice.
// The repository calls this once per persist and writes events to the outbox in the same tx.
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
