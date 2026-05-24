// Package batch defines the Batch aggregate — a per-Product
// manufacturing batch carrying running stock-on-hand.
//
// Per BRD §6.5 + ADR 0061: Batch is its OWN aggregate root (not an
// entity inside Product), referenced by ProductID, with explicit
// optimistic-concurrency token. Vernon IDDD ch.10 rationale:
//   - Batches are high-contention during stock movements (Inbound /
//     Outbound / Adjustment from concurrent Order + PurchaseOrder
//     flows). Loading the whole Product aggregate to mutate one
//     batch's quantity would lock more than needed.
//   - StockMovement is a separate append-only ledger aggregate; both
//     write to Batch.quantity_on_hand transactionally. Splitting Batch
//     out keeps the Product master read-mostly + small.
//
// Tenant-scoped. Composite-FK at the DB level guarantees same-tenant
// linkage to Product (anti-mix-up pattern per database.md).
//
// Optimistic concurrency: explicit `version int64` token (Vernon
// IDDD ch.10 + EF Core canon). Repository writes
// `UPDATE ... WHERE version = $current` + treats rows-affected = 0
// as a conflict — surface as ErrConcurrencyConflict for the app
// layer's retry loop.
package batch

import (
	"fmt"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// Field bounds (mirror migration CHECK constraints).
const (
	BatchNumberMinLen        = 1
	BatchNumberMaxLen        = 100
	ManufacturerNameMaxLen   = 200
	ManufacturingLicenceMax  = 100
)

// MovementType is the closed-set classifier on a single stock-movement
// per BRD §6.5. The Batch aggregate cares about it because Inbound +
// Outbound + Adjustment mutate quantity_on_hand while Reservation +
// Release are non-mutating (Orders module bookkeeping).
type MovementType string

// Closed-set movement types — wire-stable lower-snake strings.
// Match the SQL CHECK constraint on inventory.stock_movements.type.
const (
	MovementInbound     MovementType = "inbound"
	MovementOutbound    MovementType = "outbound"
	MovementAdjustment  MovementType = "adjustment"
	MovementReservation MovementType = "reservation"
	MovementRelease     MovementType = "release"
)

// IsValid reports whether m is in the closed catalogue.
func (m MovementType) IsValid() bool {
	switch m {
	case MovementInbound, MovementOutbound, MovementAdjustment,
		MovementReservation, MovementRelease:
		return true
	default:
		return false
	}
}

// IsMutating reports whether m changes quantity_on_hand.
// Reservation + Release are audit-only (the future Orders module
// tracks reserved qty separately).
func (m MovementType) IsMutating() bool {
	switch m {
	case MovementInbound, MovementOutbound, MovementAdjustment:
		return true
	default:
		return false
	}
}

// ErrInvalid is the sentinel for invariant violations on construction
// + ApplyMovement input validation.
var ErrInvalid = errs.New(errs.KindInvalidInput, "batch", "invalid batch")

// ErrInsufficientStock — Outbound quantity exceeds quantity_on_hand.
var ErrInsufficientStock = errs.New(errs.KindConflict, "batch", "insufficient stock for outbound movement")

// ErrExpired — Inbound rejected because the batch's expiry date is in
// the past. Outbound from an expired batch IS allowed (write-off /
// disposal) — the gate is one-way.
var ErrExpired = errs.New(errs.KindConflict, "batch", "batch has expired; inbound movements rejected")

// ErrDeleted — operation rejected on a soft-deleted batch.
var ErrDeleted = errs.New(errs.KindConflict, "batch", "batch is deleted")

// ErrConcurrencyConflict — the optimistic concurrency check failed:
// another writer modified the batch between load + persist. Adapter
// surfaces this; the application handler retries with the fresh row.
var ErrConcurrencyConflict = errs.New(errs.KindConflict, "batch", "concurrent modification; retry")

// ID is the Batch primary key (UUIDv7 string form).
type ID string

// IsZero reports whether ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// Spec is the wire-stable input to [New].
type Spec struct {
	BatchNumber                string
	ManufactureDate            time.Time
	ExpiryDate                 time.Time
	ManufacturerName           string
	ManufacturingLicenceNumber string
	MRPPaise                   int64
	PurchasePricePaise         int64
}

// Batch is the aggregate root.
//
// Invariants enforced by [New] + [ApplyMovement]:
//   - id + productID + tenantID non-zero
//   - batchNumber trimmed 1..100
//   - manufacturerName trimmed 1..200
//   - manufacturingLicenceNumber trimmed 1..100
//   - mrpPaise >= 0 (Stripe canon: int64 minor units, never float)
//   - purchasePricePaise >= 0
//   - manufactureDate + expiryDate non-zero; expiry > manufacture
//   - quantityOnHand >= 0 at all times
type Batch struct {
	id                         ID
	productID                  product.ID
	tenantID                   tenant.ID
	batchNumber                string
	manufactureDate            time.Time
	expiryDate                 time.Time
	manufacturerName           string
	manufacturingLicenceNumber string
	mrpPaise                   int64
	purchasePricePaise         int64
	quantityOnHand             int64
	version                    int64
	createdAt                  time.Time
	updatedAt                  time.Time
	deleted                    bool
	deletedAt                  time.Time
	deletedBy                  string
	events                     []Event
}

// New constructs a brand-new Batch with quantity_on_hand = 0 + version = 0.
// Returns ErrInvalid (wrapped) on invariant violation. Emits AddedEvent.
//
// actorID is the membership that added the batch — populates
// AddedEvent.ActorID for the integration mapper.
//
// `now` is the explicit instant for createdAt + updatedAt + event
// timestamp. Per the clock-injection refactor (post-Wave-9), the
// aggregate carries NO temporal dependency — time flows in at every
// call site so multiple aggregates touched in one handler share the
// same instant for audit consistency.
func New(id ID, productID product.ID, tenantID tenant.ID, actorID membership.ID, spec Spec, now time.Time) (*Batch, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if productID.IsZero() {
		return nil, fmt.Errorf("%w: product_id required", ErrInvalid)
	}
	if tenantID.IsZero() {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalid)
	}
	if actorID.IsZero() {
		return nil, fmt.Errorf("%w: actor_id required", ErrInvalid)
	}
	bn := strings.TrimSpace(spec.BatchNumber)
	if bn == "" {
		return nil, fmt.Errorf("%w: batch_number required", ErrInvalid)
	}
	if len(bn) < BatchNumberMinLen || len(bn) > BatchNumberMaxLen {
		return nil, fmt.Errorf("%w: batch_number length %d not in [%d,%d]",
			ErrInvalid, len(bn), BatchNumberMinLen, BatchNumberMaxLen)
	}
	man := strings.TrimSpace(spec.ManufacturerName)
	if man == "" {
		return nil, fmt.Errorf("%w: manufacturer_name required", ErrInvalid)
	}
	if len(man) > ManufacturerNameMaxLen {
		return nil, fmt.Errorf("%w: manufacturer_name too long (max %d)", ErrInvalid, ManufacturerNameMaxLen)
	}
	ml := strings.TrimSpace(spec.ManufacturingLicenceNumber)
	if ml == "" {
		return nil, fmt.Errorf("%w: manufacturing_licence_number required", ErrInvalid)
	}
	if len(ml) > ManufacturingLicenceMax {
		return nil, fmt.Errorf("%w: manufacturing_licence_number too long (max %d)", ErrInvalid, ManufacturingLicenceMax)
	}
	if spec.MRPPaise < 0 {
		return nil, fmt.Errorf("%w: mrp_paise must be >= 0 (got %d)", ErrInvalid, spec.MRPPaise)
	}
	if spec.PurchasePricePaise < 0 {
		return nil, fmt.Errorf("%w: purchase_price_paise must be >= 0 (got %d)", ErrInvalid, spec.PurchasePricePaise)
	}
	if spec.ManufactureDate.IsZero() {
		return nil, fmt.Errorf("%w: manufacture_date required", ErrInvalid)
	}
	if spec.ExpiryDate.IsZero() {
		return nil, fmt.Errorf("%w: expiry_date required", ErrInvalid)
	}
	if !spec.ExpiryDate.After(spec.ManufactureDate) {
		return nil, fmt.Errorf("%w: expiry_date must be after manufacture_date", ErrInvalid)
	}
	now = now.UTC()
	b := &Batch{
		id:                         id,
		productID:                  productID,
		tenantID:                   tenantID,
		batchNumber:                bn,
		manufactureDate:            spec.ManufactureDate.UTC(),
		expiryDate:                 spec.ExpiryDate.UTC(),
		manufacturerName:           man,
		manufacturingLicenceNumber: ml,
		mrpPaise:                   spec.MRPPaise,
		purchasePricePaise:         spec.PurchasePricePaise,
		quantityOnHand:             0,
		version:                    0,
		createdAt:                  now,
		updatedAt:                  now,
	}
	b.recordEvent(AddedEvent{
		BatchID:        id,
		ProductID:      productID,
		TenantID:       tenantID,
		ActorID:        actorID,
		BatchNumber:    bn,
		ExpiryDate:     b.expiryDate,
		QuantityOnHand: 0,
		At:             now,
	})
	return b, nil
}

// ----- Getters --------------------------------------------------------------

// ID returns the Batch's primary key.
func (b *Batch) ID() ID { return b.id }

// ProductID returns the FK to [product.Product].
func (b *Batch) ProductID() product.ID { return b.productID }

// TenantID returns the FK to [tenant.Tenant].
func (b *Batch) TenantID() tenant.ID { return b.tenantID }

// BatchNumber returns the manufacturer-supplied batch identifier.
func (b *Batch) BatchNumber() string { return b.batchNumber }

// ManufactureDate returns the date of manufacture (UTC, date-only semantics).
func (b *Batch) ManufactureDate() time.Time { return b.manufactureDate }

// ExpiryDate returns the date past which the batch is expired (UTC).
func (b *Batch) ExpiryDate() time.Time { return b.expiryDate }

// ManufacturerName returns the manufacturer's declared name.
func (b *Batch) ManufacturerName() string { return b.manufacturerName }

// ManufacturingLicenceNumber returns the Indian manufacturing-licence
// number (Drug Licence Form 25 / 28 / 28A).
func (b *Batch) ManufacturingLicenceNumber() string { return b.manufacturingLicenceNumber }

// MRPPaise returns the Maximum Retail Price in paise (int64 minor units;
// 1 INR = 100 paise) per Stripe canon (never float).
func (b *Batch) MRPPaise() int64 { return b.mrpPaise }

// PurchasePricePaise returns the actual purchase cost in paise.
func (b *Batch) PurchasePricePaise() int64 { return b.purchasePricePaise }

// QuantityOnHand returns the current stock count (running total updated
// by ApplyMovement).
func (b *Batch) QuantityOnHand() int64 { return b.quantityOnHand }

// Version returns the optimistic-concurrency token. The adapter writes
// `WHERE version = $current` + bumps to current+1 on UPDATE; mismatch
// surfaces as ErrConcurrencyConflict.
func (b *Batch) Version() int64 { return b.version }

// IsDeleted reports whether the batch has been soft-deleted.
func (b *Batch) IsDeleted() bool { return b.deleted }

// DeletedAt returns the soft-delete timestamp.
func (b *Batch) DeletedAt() time.Time { return b.deletedAt }

// DeletedBy returns the membership id (or operator id) that performed
// the soft-delete.
func (b *Batch) DeletedBy() string { return b.deletedBy }

// CreatedAt returns the creation timestamp.
func (b *Batch) CreatedAt() time.Time { return b.createdAt }

// UpdatedAt returns the most-recent mutation timestamp.
func (b *Batch) UpdatedAt() time.Time { return b.updatedAt }

// IsExpired reports whether the batch's expiry date is in the past
// relative to the supplied `now`. Caller passes the same instant used
// for the surrounding operation so expiry decisions stay deterministic
// inside a multi-aggregate handler invocation.
func (b *Batch) IsExpired(now time.Time) bool {
	return !b.expiryDate.After(now)
}

// ----- State transitions ----------------------------------------------------

// ApplyMovement mutates quantity_on_hand per the given movement type +
// quantity. Bumps the optimistic-concurrency version on every successful
// mutation (including non-mutating Reservation / Release, so audit-trail
// row counts line up with version increments).
//
// quantity is the SIGNED magnitude:
//   - Inbound / Reservation / Release: quantity MUST be > 0; rejected otherwise.
//   - Outbound: quantity MUST be > 0; subtracted from on-hand.
//   - Adjustment: quantity may be any non-zero (negative for shrinkage,
//     positive for count correction). Zero rejected.
//
// Rejections:
//   - ErrDeleted        — batch is soft-deleted.
//   - ErrExpired        — Inbound against an expired batch.
//   - ErrInsufficientStock — Outbound would drive on-hand negative.
//   - ErrInvalid        — bad type / zero quantity / negative on Inbound etc.
//
// No domain event emitted from ApplyMovement — the StockMovement
// aggregate emits the canonical movement event. Batch just updates its
// own state inside the same tx the StockMovement insert runs under.
//
// `now` is the explicit instant for the expiry comparison + the
// updatedAt timestamp — caller threads the same value into the paired
// StockMovement.New so the ledger row + the batch row share one moment
// in audit / reconciliation queries.
func (b *Batch) ApplyMovement(movement MovementType, quantity int64, now time.Time) error {
	if b.deleted {
		return fmt.Errorf("%w: %s", ErrDeleted, b.id)
	}
	if !movement.IsValid() {
		return fmt.Errorf("%w: unknown movement type %q", ErrInvalid, movement)
	}
	switch movement {
	case MovementInbound:
		if quantity <= 0 {
			return fmt.Errorf("%w: inbound quantity must be > 0 (got %d)", ErrInvalid, quantity)
		}
		if b.IsExpired(now) {
			return fmt.Errorf("%w: expired %s", ErrExpired, b.expiryDate.Format("2006-01-02"))
		}
		b.quantityOnHand += quantity
	case MovementOutbound:
		if quantity <= 0 {
			return fmt.Errorf("%w: outbound quantity must be > 0 (got %d)", ErrInvalid, quantity)
		}
		if quantity > b.quantityOnHand {
			return fmt.Errorf("%w: requested %d, on-hand %d", ErrInsufficientStock, quantity, b.quantityOnHand)
		}
		b.quantityOnHand -= quantity
	case MovementAdjustment:
		if quantity == 0 {
			return fmt.Errorf("%w: adjustment quantity must be non-zero", ErrInvalid)
		}
		next := b.quantityOnHand + quantity
		if next < 0 {
			return fmt.Errorf("%w: adjustment would drive on-hand negative (current %d, delta %d)",
				ErrInsufficientStock, b.quantityOnHand, quantity)
		}
		b.quantityOnHand = next
	case MovementReservation, MovementRelease:
		if quantity <= 0 {
			return fmt.Errorf("%w: %s quantity must be > 0 (got %d)", ErrInvalid, movement, quantity)
		}
		// Non-mutating: the future Orders module tracks reserved-qty
		// separately. We bump version anyway so concurrent writers
		// surface the conflict.
	}
	b.version++
	b.updatedAt = now.UTC()
	return nil
}

// SoftDelete marks the batch deleted, recording who did it. Idempotent.
//
// CALLER INVARIANT (application layer): SoftDelete should be REJECTED
// when quantity_on_hand > 0. The domain doesn't enforce that — the
// SoftDeleteBatchHandler does (so the handler can return a friendly
// 409 message rather than the bare invariant).
func (b *Batch) SoftDelete(actorID membership.ID, now time.Time) error {
	if b.deleted {
		return nil
	}
	if actorID.IsZero() {
		return fmt.Errorf("%w: actor_id required for audit", ErrInvalid)
	}
	now = now.UTC()
	b.deleted = true
	b.deletedAt = now
	b.deletedBy = actorID.String()
	b.updatedAt = now
	b.version++
	return nil
}

// ----- Persistence DTO ------------------------------------------------------

// Snapshot is the persistence-layer projection consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID                         ID
	ProductID                  product.ID
	TenantID                   tenant.ID
	BatchNumber                string
	ManufactureDate            time.Time
	ExpiryDate                 time.Time
	ManufacturerName           string
	ManufacturingLicenceNumber string
	MRPPaise                   int64
	PurchasePricePaise         int64
	QuantityOnHand             int64
	Version                    int64
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
	IsDeleted                  bool
	DeletedAt                  time.Time
	DeletedBy                  string
}

// UnmarshalFromDB re-hydrates a Batch from persistence. Repository-only.
// Does NOT re-validate (TDL canon). Does NOT emit events.
func UnmarshalFromDB(s Snapshot) *Batch {
	return &Batch{
		id:                         s.ID,
		productID:                  s.ProductID,
		tenantID:                   s.TenantID,
		batchNumber:                s.BatchNumber,
		manufactureDate:            s.ManufactureDate.UTC(),
		expiryDate:                 s.ExpiryDate.UTC(),
		manufacturerName:           s.ManufacturerName,
		manufacturingLicenceNumber: s.ManufacturingLicenceNumber,
		mrpPaise:                   s.MRPPaise,
		purchasePricePaise:         s.PurchasePricePaise,
		quantityOnHand:             s.QuantityOnHand,
		version:                    s.Version,
		createdAt:                  s.CreatedAt,
		updatedAt:                  s.UpdatedAt,
		deleted:                    s.IsDeleted,
		deletedAt:                  s.DeletedAt,
		deletedBy:                  s.DeletedBy,
	}
}

// ----- Event handling -------------------------------------------------------

// PullEvents drains the recorded domain events.
func (b *Batch) PullEvents() []Event {
	if len(b.events) == 0 {
		return nil
	}
	out := b.events
	b.events = nil
	return out
}

func (b *Batch) recordEvent(e Event) {
	b.events = append(b.events, e)
}
