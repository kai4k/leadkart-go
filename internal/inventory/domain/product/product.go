// Package product defines the Product aggregate — Inventory catalog master per BRD §6.5 + ADR 0061.
//
// Tenant-scoped; identified by [ID] (UUIDv7). Carries the fields every Batch and order line
// references: SKU, name, dosage form, pack size, HSN code, gst_rate_bps (basis points, never float
// per Stripe canon), manufacturer and is_active (a product is made inactive, never deleted: ADR 0073).
//
// Construct via [New] (invariants enforced) or [UnmarshalFromDB] (repo re-hydration, no re-validation
// per TDL Wild Workouts canon). Batches reference Products by ID only (Vernon IDDD ch.10);
// cross-aggregate consistency via same-tx outbox in the application layer.
package product

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/hsncode"
	"github.com/leadkart/leadkart-go/internal/common/textlimit"
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
	HSNCodeMinLength    = 4 // shortest HSN code; the full rule lives in common/hsncode
	HSNCodeMaxLength    = 8 // longest HSN code (the DB CHECK still allows up to 10: rows stored before the rule)
	ManufacturerMaxLen  = 200
	GSTRateBpsMin       = 0     // 0%
	GSTRateBpsMax       = 10000 // 100% — practical ceiling; basis points
	// ReorderLevelMin floors the reorder_level field. 0 disables the
	// per-product reorder alert (the ReorderScanJob skips zero rows).
	ReorderLevelMin = 0
	// ReorderLevelMax is the largest value the int column holds.
	ReorderLevelMax = math.MaxInt32
	// ExpiryAlertThresholdDaysMin / Default — per BRD §6.5.
	ExpiryAlertThresholdDaysMin     = 0
	ExpiryAlertThresholdDaysDefault = 90
	// ExpiryAlertThresholdDaysMax is 100 years: the expiry scan adds the
	// threshold to a date, so it must stay inside the date range.
	ExpiryAlertThresholdDaysMax = 36500
	// ProductCategory bounds — mirror the migration's CHECK + DEFAULT.
	ProductCategoryMinLength = 1
	ProductCategoryMaxLength = 64
	ProductCategoryDefault   = "General"

	// Optional catalogue text; mirror the migration CHECKs.
	GenericNameMaxLen      = 200
	CompositionMaxLen      = 1000
	StorageConditionMaxLen = 200
	// DefaultSaleRateMaxPaise caps the default sale rate (Rs 100 crore, the line cap).
	DefaultSaleRateMaxPaise = 100_00_00_000 * 100
)

// ErrInvalid is returned (wrapped via %w) by [New] and [Update] on invariant violation.
var ErrInvalid = errs.New(errs.KindInvalidInput, "product", "invalid product")

// ErrInactive is returned when an action that needs an active product (a new batch) targets an
// inactive one. Inactive products stay readable and keep their batches and stock; they are
// never deleted (ADR 0073).
var ErrInactive = errs.New(errs.KindConflict, "product", "product is inactive")

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
	// GSTRateID is the entry of the company GST rate list the product uses; the
	// application layer resolves it (GSTRateBps is that entry's rate). Optional at
	// the domain: a UUID when given.
	GSTRateID string
	// DefaultSaleRatePaise is the rate offered on a quotation line (0: none).
	DefaultSaleRatePaise int64
	GenericName          string
	Composition          string
	StorageCondition     string
	// DrugScheduleClassID is the company drug schedule class (an id from its own
	// list); empty means the product carries none. Nothing here knows any class.
	DrugScheduleClassID string
}

// UpdateSpec is the partial-update input for [Product.Update].
// Only non-nil fields are applied; matching values are no-ops (no event emitted).
// ChangedFields on the resulting event surfaces the diff.
type UpdateSpec struct {
	Name                     *string
	GSTRateBps               *int
	Manufacturer             *string
	ReorderLevel             *int
	ExpiryAlertThresholdDays *int
	ProductCategory          *string
	// GSTRateID with GSTRateBps moves the product to another entry of the company
	// rate list (the application layer resolves both).
	GSTRateID            *string
	DefaultSaleRatePaise *int64
	GenericName          *string
	Composition          *string
	StorageCondition     *string
	// DrugScheduleClassID sets the class; a pointer to "" clears it.
	DrugScheduleClassID *string
}

// Product is the aggregate root.
//
// Invariants enforced by [New] and [Update]:
//   - id + tenantID non-zero
//   - SKU trimmed + upper-cased, 1..SKUMaxLength
//   - Name trimmed, 1..NameMaxLength
//   - DosageForm + PackSize trimmed, length-bounded
//   - HSNCode trimmed, 4, 6 or 8 digits, leading zero allowed (common/hsncode)
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
	gstRateID                string
	defaultSaleRatePaise     int64
	genericName              string
	composition              string
	storageCondition         string
	drugScheduleClassID      string
	createdAt                time.Time
	updatedAt                time.Time
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
	hsnCode, err := hsncode.New(spec.HSNCode) // the one HSN rule: trimmed, 4/6/8 digits, leading zero allowed
	if err != nil {
		return nil, fmt.Errorf("%w: hsn_code: %w", ErrInvalid, err)
	}
	hsn := hsnCode.String()
	if spec.GSTRateBps < GSTRateBpsMin || spec.GSTRateBps > GSTRateBpsMax {
		return nil, fmt.Errorf("%w: gst_rate_bps %d not in [%d,%d]",
			ErrInvalid, spec.GSTRateBps, GSTRateBpsMin, GSTRateBpsMax)
	}
	manufacturer := strings.TrimSpace(spec.Manufacturer)
	if err := textlimit.Chars("manufacturer", manufacturer, ManufacturerMaxLen); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := validateAlertLevels(spec.ReorderLevel, spec.ExpiryAlertThresholdDays); err != nil {
		return nil, err
	}
	category := strings.TrimSpace(spec.ProductCategory)
	if category == "" {
		category = ProductCategoryDefault
	}
	if err := textlimit.Chars("product_category", category, ProductCategoryMaxLength); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	extra, err := validateCatalogue(spec)
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	p := &Product{
		gstRateID:                extra.gstRateID,
		defaultSaleRatePaise:     spec.DefaultSaleRatePaise,
		genericName:              extra.genericName,
		composition:              extra.composition,
		storageCondition:         extra.storageCondition,
		drugScheduleClassID:      extra.drugScheduleClassID,
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

// GSTRateID returns the id of the company GST rate list entry the product uses
// (empty for a product that predates the list).
func (p *Product) GSTRateID() string { return p.gstRateID }

// DefaultSaleRatePaise returns the rate offered on a quotation line (0: none).
func (p *Product) DefaultSaleRatePaise() int64 { return p.defaultSaleRatePaise }

// GenericName returns the optional generic name.
func (p *Product) GenericName() string { return p.genericName }

// Composition returns the optional composition text.
func (p *Product) Composition() string { return p.composition }

// StorageCondition returns the optional storage condition text.
func (p *Product) StorageCondition() string { return p.storageCondition }

// DrugScheduleClassID returns the company drug schedule class the product
// carries (empty when none).
func (p *Product) DrugScheduleClassID() string { return p.drugScheduleClassID }

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

// HSNCode returns the Indian HSN code (4, 6 or 8 digits for a product created
// under the current rule).
func (p *Product) HSNCode() string { return p.hsnCode }

// GSTRateBps returns the GST rate in basis points (1200 = 12.00%; never float).
func (p *Product) GSTRateBps() int { return p.gstRateBps }

// Manufacturer returns the declared manufacturer name (may be empty).
func (p *Product) Manufacturer() string { return p.manufacturer }

// IsActive reports whether the product is active. An inactive product is still readable and
// keeps its batches and stock, but takes no new batch (ADR 0073).
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

// CreatedAt returns the creation timestamp (immutable).
func (p *Product) CreatedAt() time.Time { return p.createdAt }

// UpdatedAt returns the most recent mutation timestamp.
func (p *Product) UpdatedAt() time.Time { return p.updatedAt }

// Update applies a partial-update spec; only non-nil fields are applied.
// Matching values are no-ops (no event emitted).
// Emits [UpdatedEvent] with ChangedFields sorted alphabetically.
// Returns [ErrInvalid] on validation failure.
func (p *Product) Update(actorID membership.ID, spec UpdateSpec, now time.Time) error {
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
	if spec.Manufacturer != nil {
		m := strings.TrimSpace(*spec.Manufacturer)
		if err := textlimit.Chars("manufacturer", m, ManufacturerMaxLen); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
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
	cat, err := p.applyCatalogueUpdates(spec)
	if err != nil {
		return err
	}
	changed = append(changed, cat...)
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
	// Both levels are checked before either is applied, so a refused update
	// leaves the aggregate as it was.
	reorder, threshold := p.reorderLevel, p.expiryAlertThresholdDays
	if spec.ReorderLevel != nil {
		reorder = *spec.ReorderLevel
	}
	if spec.ExpiryAlertThresholdDays != nil {
		threshold = *spec.ExpiryAlertThresholdDays
	}
	if err := validateAlertLevels(reorder, threshold); err != nil {
		return nil, err
	}
	if spec.ReorderLevel != nil {
		v := *spec.ReorderLevel
		if v != p.reorderLevel {
			p.reorderLevel = v
			changed = append(changed, "reorder_level")
		}
	}
	if spec.ExpiryAlertThresholdDays != nil {
		v := *spec.ExpiryAlertThresholdDays
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
		if err := textlimit.Chars("product_category", c, ProductCategoryMaxLength); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		if c != p.productCategory {
			p.productCategory = c
			changed = append(changed, "product_category")
		}
	}
	return changed, nil
}

// applyCatalogueUpdates applies the G2 catalogue fields: the GST rate list entry,
// the default sale rate, the optional texts and the drug schedule class. All of
// them are validated before any is applied, so a refused update changes nothing.
func (p *Product) applyCatalogueUpdates(spec UpdateSpec) ([]string, error) {
	next := Spec{
		GSTRateID: p.gstRateID, DefaultSaleRatePaise: p.defaultSaleRatePaise, GenericName: p.genericName,
		Composition: p.composition, StorageCondition: p.storageCondition, DrugScheduleClassID: p.drugScheduleClassID,
	}
	if spec.GSTRateID != nil {
		next.GSTRateID = *spec.GSTRateID
	}
	if spec.DefaultSaleRatePaise != nil {
		next.DefaultSaleRatePaise = *spec.DefaultSaleRatePaise
	}
	if spec.GenericName != nil {
		next.GenericName = *spec.GenericName
	}
	if spec.Composition != nil {
		next.Composition = *spec.Composition
	}
	if spec.StorageCondition != nil {
		next.StorageCondition = *spec.StorageCondition
	}
	if spec.DrugScheduleClassID != nil {
		next.DrugScheduleClassID = *spec.DrugScheduleClassID
	}
	v, err := validateCatalogue(next)
	if err != nil {
		return nil, err
	}
	var changed []string
	set := func(field string, differs bool, apply func()) {
		if differs {
			apply()
			changed = append(changed, field)
		}
	}
	set("gst_rate_id", v.gstRateID != p.gstRateID, func() { p.gstRateID = v.gstRateID })
	set("default_sale_rate_paise", next.DefaultSaleRatePaise != p.defaultSaleRatePaise, func() { p.defaultSaleRatePaise = next.DefaultSaleRatePaise })
	set("generic_name", v.genericName != p.genericName, func() { p.genericName = v.genericName })
	set("composition", v.composition != p.composition, func() { p.composition = v.composition })
	set("storage_condition", v.storageCondition != p.storageCondition, func() { p.storageCondition = v.storageCondition })
	set("drug_schedule", v.drugScheduleClassID != p.drugScheduleClassID, func() { p.drugScheduleClassID = v.drugScheduleClassID })
	return changed, nil
}

type catalogue struct {
	gstRateID, genericName, composition, storageCondition, drugScheduleClassID string
}

// validateCatalogue checks the G2 catalogue fields of a spec and returns their
// stored (trimmed) form.
func validateCatalogue(spec Spec) (catalogue, error) {
	var c catalogue
	for _, f := range []struct {
		name, in string
		out      *string
	}{
		{"gst_rate_id", spec.GSTRateID, &c.gstRateID},
		{"drug_schedule", spec.DrugScheduleClassID, &c.drugScheduleClassID},
	} {
		v := strings.TrimSpace(f.in)
		if v != "" {
			if _, err := uuid.Parse(v); err != nil {
				return catalogue{}, fmt.Errorf("%w: %s is not a valid uuid", ErrInvalid, f.name)
			}
		}
		*f.out = v
	}
	if spec.DefaultSaleRatePaise < 0 || spec.DefaultSaleRatePaise > DefaultSaleRateMaxPaise {
		return catalogue{}, fmt.Errorf("%w: default_sale_rate_paise %d not in [0,%d]", ErrInvalid, spec.DefaultSaleRatePaise, DefaultSaleRateMaxPaise)
	}
	for _, f := range []struct {
		name string
		in   string
		max  int
		out  *string
	}{
		{"generic_name", spec.GenericName, GenericNameMaxLen, &c.genericName},
		{"composition", spec.Composition, CompositionMaxLen, &c.composition},
		{"storage_condition", spec.StorageCondition, StorageConditionMaxLen, &c.storageCondition},
	} {
		v := strings.TrimSpace(f.in)
		if err := textlimit.Chars(f.name, v, f.max); err != nil {
			return catalogue{}, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		*f.out = v
	}
	return c, nil
}

// Activate makes the product active again and emits [ActivatedEvent]. Idempotent: an already
// active product is a no-op (no event).
func (p *Product) Activate(actorID membership.ID, now time.Time) error {
	if actorID.IsZero() {
		return fmt.Errorf("%w: actorID required", ErrInvalid)
	}
	if p.isActive {
		return nil
	}
	now = now.UTC()
	p.isActive = true
	p.updatedAt = now
	p.recordEvent(ActivatedEvent{ProductID: p.id, TenantID: p.tenantID, ActorID: actorID, At: now})
	return nil
}

// Deactivate makes the product inactive and emits [DeactivatedEvent]. A product is never
// deleted: it stays readable and keeps its batches and stock (ADR 0073). Idempotent: an already
// inactive product is a no-op (no event).
func (p *Product) Deactivate(actorID membership.ID, now time.Time) error {
	if actorID.IsZero() {
		return fmt.Errorf("%w: actorID required", ErrInvalid)
	}
	if !p.isActive {
		return nil
	}
	now = now.UTC()
	p.isActive = false
	p.updatedAt = now
	p.recordEvent(DeactivatedEvent{ProductID: p.id, TenantID: p.tenantID, ActorID: actorID, At: now})
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
	GSTRateID                string
	DefaultSaleRatePaise     int64
	GenericName              string
	Composition              string
	StorageCondition         string
	DrugScheduleClassID      string
	CreatedAt                time.Time
	UpdatedAt                time.Time
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
		gstRateID:                s.GSTRateID,
		defaultSaleRatePaise:     s.DefaultSaleRatePaise,
		genericName:              s.GenericName,
		composition:              s.Composition,
		storageCondition:         s.StorageCondition,
		drugScheduleClassID:      s.DrugScheduleClassID,
		createdAt:                s.CreatedAt,
		updatedAt:                s.UpdatedAt,
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

// validateAlertLevels bounds the reorder level and the expiry-alert threshold.
func validateAlertLevels(reorderLevel, thresholdDays int) error {
	if reorderLevel < ReorderLevelMin || reorderLevel > ReorderLevelMax {
		return fmt.Errorf("%w: reorder_level %d not in [%d,%d]",
			ErrInvalid, reorderLevel, ReorderLevelMin, ReorderLevelMax)
	}
	if thresholdDays < ExpiryAlertThresholdDaysMin || thresholdDays > ExpiryAlertThresholdDaysMax {
		return fmt.Errorf("%w: expiry_alert_threshold_days %d not in [%d,%d]",
			ErrInvalid, thresholdDays, ExpiryAlertThresholdDaysMin, ExpiryAlertThresholdDaysMax)
	}
	return nil
}

func validateSKU(raw string) (string, error) {
	trimmed := strings.ToUpper(strings.TrimSpace(raw))
	if trimmed == "" {
		return "", fmt.Errorf("%w: sku required", ErrInvalid)
	}
	// The column CHECK counts characters (length()), so the limit does too; the
	// same check refuses U+0000, which a text column cannot hold.
	if err := textlimit.Chars("sku", trimmed, SKUMaxLength); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return trimmed, nil
}

func validateBoundedTrim(raw, field string, min, max int) (string, error) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", fmt.Errorf("%w: %s required", ErrInvalid, field)
	}
	// Limits are in CHARACTERS, the unit of the column CHECK (length()); the
	// same check refuses U+0000, which a text column cannot hold.
	if err := textlimit.Chars(field, t, max); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if n := utf8.RuneCountInString(t); n < min {
		return "", fmt.Errorf("%w: %s length %d below the minimum %d", ErrInvalid, field, n, min)
	}
	return t, nil
}
