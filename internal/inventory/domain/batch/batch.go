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

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/expiry"
	"github.com/leadkart/leadkart-go/internal/common/textlimit"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

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

// Field bounds (mirror migration CHECK constraints).
const (
	BatchNumberMinLen       = 1
	BatchNumberMaxLen       = 100
	ManufacturerNameMaxLen  = 200
	ManufacturingLicenceMax = 100
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
	// MovementQuarantine sets free units apart as "Not for sale".
	MovementQuarantine MovementType = "quarantine"
	// MovementReleaseQuarantine returns quarantined units to free stock.
	MovementReleaseQuarantine MovementType = "release_quarantine"
	// MovementWriteOff removes quarantined units from the books. The batch row
	// and the ledger keep the history: it is a state change, never a delete.
	MovementWriteOff MovementType = "write_off"
)

// IsValid reports whether m is in the closed catalogue.
func (m MovementType) IsValid() bool {
	switch m {
	case MovementInbound, MovementOutbound, MovementAdjustment,
		MovementReservation, MovementRelease,
		MovementQuarantine, MovementReleaseQuarantine, MovementWriteOff:
		return true
	default:
		return false
	}
}

// IsMutating reports whether m changes quantity_on_hand specifically.
// Reservation + Release move quantity_reserved (not quantity_on_hand),
// so they are NOT on-hand-mutating — but they DO mutate batch state
// (the reserved counter + version). See ApplyMovement for the
// true-hold reservation semantics adopted per ADR 0063 §4.
func (m MovementType) IsMutating() bool {
	switch m {
	case MovementInbound, MovementOutbound, MovementAdjustment, MovementWriteOff:
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

// ErrInsufficientAvailable — a Reservation requested more than the batch's
// AVAILABLE quantity (quantity_on_hand − quantity_reserved). This is the
// true-hold oversell guard (ADR 0063 §4): two orders cannot both reserve the
// same physical units.
var ErrInsufficientAvailable = errs.New(errs.KindConflict, "batch", "insufficient available stock to reserve")

// ErrReleaseExceedsReserved — a Release requested more than the batch's
// currently-reserved quantity (a programmer/saga bug; releases must mirror
// prior reservations exactly).
var ErrReleaseExceedsReserved = errs.New(errs.KindInvalidInput, "batch", "release exceeds reserved quantity")

// ErrExpired — the movement is refused because the batch is past its printed
// expiry month: stock cannot be received into it or released from quarantine
// (the legal limit no setting moves). Outbound, quarantine and write-off stay
// allowed so expired stock can be set apart and disposed of.
var ErrExpired = errs.New(errs.KindConflict, "batch", "batch is past its printed expiry; the movement is refused")

// ErrInsufficientFree — a quarantine requested more than the batch's free units
// (on hand minus reserved minus already quarantined).
var ErrInsufficientFree = errs.New(errs.KindConflict, "batch", "insufficient free stock to quarantine")

// ErrInsufficientQuarantined — a release or write-off requested more than the
// batch's quarantined units.
var ErrInsufficientQuarantined = errs.New(errs.KindConflict, "batch", "insufficient quarantined stock")

// ErrManufactureDateInFuture — the manufacture date is after the company business
// day: a batch cannot have been made tomorrow.
var ErrManufactureDateInFuture = errs.New(errs.KindInvalidInput, "batch", "manufacture_date is in the future")

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
//   - quantityReserved >= 0 and quantityReserved <= quantityOnHand at all
//     times (available = on-hand − reserved is never negative)
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
	quantityReserved           int64
	quantityQuarantined        int64
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
	if err := validateUUID("id", id.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("product_id", productID.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("tenant_id", tenantID.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("actor_id", actorID.String()); err != nil {
		return nil, err
	}
	bn := strings.TrimSpace(spec.BatchNumber)
	if bn == "" {
		return nil, fmt.Errorf("%w: batch_number required", ErrInvalid)
	}
	// Limits are in CHARACTERS, the unit of the column CHECKs (length()); the
	// same check refuses U+0000, which a text column cannot hold.
	if err := textlimit.Chars("batch_number", bn, BatchNumberMaxLen); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	man := strings.TrimSpace(spec.ManufacturerName)
	if man == "" {
		return nil, fmt.Errorf("%w: manufacturer_name required", ErrInvalid)
	}
	if err := textlimit.Chars("manufacturer_name", man, ManufacturerNameMaxLen); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	ml := strings.TrimSpace(spec.ManufacturingLicenceNumber)
	if ml == "" {
		return nil, fmt.Errorf("%w: manufacturing_licence_number required", ErrInvalid)
	}
	if err := textlimit.Chars("manufacturing_licence_number", ml, ManufacturingLicenceMax); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
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
	if !expiry.IsMonthEnd(spec.ExpiryDate) {
		return nil, fmt.Errorf("%w: expiry_date must be the last day of the printed month", ErrInvalid)
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

// QuantityOnHand returns the current physical stock count (running total
// updated by ApplyMovement Inbound/Outbound/Adjustment).
func (b *Batch) QuantityOnHand() int64 { return b.quantityOnHand }

// QuantityReserved returns the quantity soft-committed to confirmed orders
// but not yet shipped (moved by Reservation/Release/consume). Never exceeds
// QuantityOnHand.
func (b *Batch) QuantityReserved() int64 { return b.quantityReserved }

// AvailableQuantity returns the stock free to reserve: on-hand minus
// reserved. This is the figure the reservation guard checks — the
// oversell-prevention invariant (ADR 0063 §4).
func (b *Batch) AvailableQuantity() int64 {
	return b.quantityOnHand - b.quantityReserved - b.quantityQuarantined
}

// QuantityQuarantined returns the units set apart as "Not for sale". They are
// part of QuantityOnHand and are never available to reserve or sell.
func (b *Batch) QuantityQuarantined() int64 { return b.quantityQuarantined }

// Buckets splits the batch's on-hand units into the five stock buckets, given
// whether the batch is still sellable under the company's expiry rule. The five
// always sum to on-hand: reserved + available + unsellable + quarantined.
//
//   - Reserved: held for orders.
//   - Quarantined: set apart as "Not for sale".
//   - Available: free units of a sellable batch.
//   - Unsellable: free units of a batch past the company's cut-off (not
//     quarantined yet): they must be set apart, never sold.
func (b *Batch) Buckets(sellable bool) Buckets {
	free := b.quantityOnHand - b.quantityReserved - b.quantityQuarantined
	out := Buckets{OnHand: b.quantityOnHand, Reserved: b.quantityReserved, Quarantined: b.quantityQuarantined}
	if sellable {
		out.Available = free
	} else {
		out.Unsellable = free
	}
	return out
}

// Buckets is the stock split of a batch or, summed, of a product.
type Buckets struct {
	OnHand      int64
	Reserved    int64
	Available   int64
	Unsellable  int64
	Quarantined int64
}

// Add returns the bucket-wise sum.
func (a Buckets) Add(o Buckets) Buckets {
	return Buckets{
		OnHand: a.OnHand + o.OnHand, Reserved: a.Reserved + o.Reserved, Available: a.Available + o.Available,
		Unsellable: a.Unsellable + o.Unsellable, Quarantined: a.Quarantined + o.Quarantined,
	}
}

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

// IsPastPrintedExpiry reports whether the batch's printed expiry month is over
// on the business day today. This is the legal limit; the company's own
// cut-off is the Window of the inventory app.
func (b *Batch) IsPastPrintedExpiry(today time.Time) bool {
	return expiry.IsPastPrintedExpiry(b.expiryDate, today)
}

// ----- State transitions ----------------------------------------------------

// ApplyMovement mutates batch state per the given movement type +
// quantity. Bumps the optimistic-concurrency version on every successful
// call (including Reservation / Release, which move quantity_reserved
// rather than quantity_on_hand).
//
// True-hold reservation (ADR 0063 §4 — supersedes the earlier audit-only
// stance): Reservation soft-commits AVAILABLE stock to a confirmed order;
// Release returns it. Outbound/Adjustment may only touch UNRESERVED stock,
// preserving the invariant 0 <= quantity_reserved <= quantity_on_hand.
// Order fulfilment consumes a reservation as a Release + an Outbound in one
// tx (reserved−q, on-hand−q), realising the hold as a physical shipment.
//
// quantity is the magnitude:
//   - Inbound / Outbound / Reservation / Release: quantity MUST be > 0.
//   - Adjustment: quantity may be any non-zero (negative for shrinkage,
//     positive for count correction). Zero rejected.
//
// Rejections:
//   - ErrDeleted              — batch is soft-deleted.
//   - ErrExpired              — Inbound against an expired batch.
//   - ErrInsufficientStock    — Outbound/Adjustment would drive on-hand below reserved.
//   - ErrInsufficientAvailable — Reservation exceeds available (on-hand − reserved).
//   - ErrReleaseExceedsReserved — Release exceeds currently-reserved.
//   - ErrInvalid              — bad type / zero quantity / negative on Inbound etc.
//
// No domain event emitted from ApplyMovement — the StockMovement
// aggregate emits the canonical movement event. Batch just updates its
// own state inside the same tx the StockMovement insert runs under.
//
// `now` is the explicit instant for the expiry comparison + the
// updatedAt timestamp — caller threads the same value into the paired
// StockMovement.New so the ledger row + the batch row share one moment
// in audit / reconciliation queries.
func (b *Batch) ApplyMovement(movement MovementType, quantity int64, now, today time.Time) error {
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
		if b.IsPastPrintedExpiry(today) {
			return fmt.Errorf("%w: printed expiry %s", ErrExpired, expiry.PrintedMonth(b.expiryDate))
		}
		b.quantityOnHand += quantity
	case MovementOutbound:
		if quantity <= 0 {
			return fmt.Errorf("%w: outbound quantity must be > 0 (got %d)", ErrInvalid, quantity)
		}
		// Outbound may only ship UNRESERVED stock — reserved units are
		// held for confirmed orders and leave via consume (Release+Outbound).
		if quantity > b.AvailableQuantity() {
			return fmt.Errorf("%w: requested %d, available %d (on-hand %d, reserved %d)",
				ErrInsufficientStock, quantity, b.AvailableQuantity(), b.quantityOnHand, b.quantityReserved)
		}
		b.quantityOnHand -= quantity
	case MovementAdjustment:
		if quantity == 0 {
			return fmt.Errorf("%w: adjustment quantity must be non-zero", ErrInvalid)
		}
		next := b.quantityOnHand + quantity
		// Adjustment-down cannot drop on-hand below the held units.
		if next < b.quantityReserved+b.quantityQuarantined {
			return fmt.Errorf("%w: adjustment would drive on-hand (%d) below reserved (%d), delta %d",
				ErrInsufficientStock, b.quantityOnHand, b.quantityReserved, quantity)
		}
		b.quantityOnHand = next
	case MovementReservation:
		if quantity <= 0 {
			return fmt.Errorf("%w: reservation quantity must be > 0 (got %d)", ErrInvalid, quantity)
		}
		// True-hold oversell guard: cannot reserve beyond available.
		if quantity > b.AvailableQuantity() {
			return fmt.Errorf("%w: requested %d, available %d (on-hand %d, reserved %d)",
				ErrInsufficientAvailable, quantity, b.AvailableQuantity(), b.quantityOnHand, b.quantityReserved)
		}
		b.quantityReserved += quantity
	case MovementRelease:
		if quantity <= 0 {
			return fmt.Errorf("%w: release quantity must be > 0 (got %d)", ErrInvalid, quantity)
		}
		if quantity > b.quantityReserved {
			return fmt.Errorf("%w: requested %d, reserved %d", ErrReleaseExceedsReserved, quantity, b.quantityReserved)
		}
		b.quantityReserved -= quantity
	default:
		if err := b.applyQuarantineMovement(movement, quantity, today); err != nil {
			return err
		}
	}
	b.version++
	b.updatedAt = now.UTC()
	return nil
}

// applyQuarantineMovement applies quarantine, release_quarantine and write_off.
func (b *Batch) applyQuarantineMovement(movement MovementType, quantity int64, today time.Time) error {
	if quantity <= 0 {
		return fmt.Errorf("%w: %s quantity must be > 0 (got %d)", ErrInvalid, movement, quantity)
	}
	switch movement {
	case MovementQuarantine:
		if quantity > b.AvailableQuantity() {
			return fmt.Errorf("%w: requested %d, free %d (on-hand %d, reserved %d, quarantined %d)",
				ErrInsufficientFree, quantity, b.AvailableQuantity(), b.quantityOnHand, b.quantityReserved, b.quantityQuarantined)
		}
		b.quantityQuarantined += quantity
	case MovementReleaseQuarantine:
		if quantity > b.quantityQuarantined {
			return fmt.Errorf("%w: requested %d, quarantined %d", ErrInsufficientQuarantined, quantity, b.quantityQuarantined)
		}
		// Stock past its printed expiry may only stay apart, go back to the
		// maker or be written off (Rule 65(17)).
		if b.IsPastPrintedExpiry(today) {
			return fmt.Errorf("%w: printed expiry %s", ErrExpired, expiry.PrintedMonth(b.expiryDate))
		}
		b.quantityQuarantined -= quantity
	case MovementWriteOff:
		if quantity > b.quantityQuarantined {
			return fmt.Errorf("%w: requested %d, quarantined %d", ErrInsufficientQuarantined, quantity, b.quantityQuarantined)
		}
		b.quantityQuarantined -= quantity
		b.quantityOnHand -= quantity
	default:
		return fmt.Errorf("%w: unknown movement type %q", ErrInvalid, movement)
	}
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
	QuantityReserved           int64
	QuantityQuarantined        int64
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
		quantityReserved:           s.QuantityReserved,
		quantityQuarantined:        s.QuantityQuarantined,
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
