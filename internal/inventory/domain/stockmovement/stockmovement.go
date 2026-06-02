// Package stockmovement defines the StockMovement aggregate — the
// append-only ledger row for every stock change on a Batch.
//
// Per BRD §6.5 + ADR 0061: StockMovement is its OWN aggregate (Vernon
// IDDD ch.7 — append-only ledgers warrant their own aggregate lifecycle
// from the thing they observe). Reasons:
//   - Audit-load-bearing: every row must remain immutable post-write
//     for forensics (DPDP / GST / Schedule-X drug-trail).
//   - Different consistency boundary: a movement INSERT runs in the same
//     tx as the parent Batch UPDATE, but the row itself is never
//     mutated again — no UpdateByID, no soft-delete.
//   - Different read pattern: the ledger is queried by (batch_id,
//     occurred_at) for the audit-log view + (tenant_id, occurred_at)
//     for the dashboard, independent of Batch reads.
//
// Tenant-scoped + composite-FK to Batch (same anti-mix-up pattern as
// role_assignments → memberships).
//
// Convention: Quantity is SIGNED:
//   - Inbound: positive
//   - Outbound: negative (Spec.Quantity caller-supplied as negative)
//   - Adjustment: any non-zero (negative for shrinkage; positive for count
//     correction)
//   - Reservation / Release: positive (audit-only; non-mutating to
//     batch.quantity_on_hand)
//
// This convention means SUM(quantity) over Inbound/Outbound/Adjustment
// rows for a batch equals batch.quantity_on_hand — useful for ledger
// reconciliation queries.
package stockmovement

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// Field bounds.
const (
	ReasonMinLength    = 1
	ReasonMaxLength    = 500
	SourceReferenceMax = 200
)

// ErrInvalid is the sentinel for invariant violations.
var ErrInvalid = errs.New(errs.KindInvalidInput, "stock_movement", "invalid stock movement")

// ID is the StockMovement primary key.
type ID string

// IsZero reports whether ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// Spec is the wire-stable input to [New]. QuantityOnHandAfter is the
// batch's new quantity_on_hand AFTER applying this movement; the handler
// derives it from the Batch aggregate's state post-ApplyMovement. We
// store it on the row so the ledger UI doesn't have to re-derive on read.
type Spec struct {
	BatchID             batch.ID
	ProductID           product.ID
	TenantID            tenant.ID
	Type                batch.MovementType
	Quantity            int64
	QuantityOnHandAfter int64
	Reason              string
	ActorMembershipID   membership.ID
	SourceReference     *string
}

// Movement is the aggregate root — an append-only ledger row.
type Movement struct {
	id                  ID
	batchID             batch.ID
	productID           product.ID
	tenantID            tenant.ID
	movementType        batch.MovementType
	quantity            int64
	quantityOnHandAfter int64
	reason              string
	actorMembershipID   membership.ID
	sourceReference     *string
	occurredAt          time.Time
	events              []Event
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

// New constructs a new ledger row. Returns ErrInvalid on invariant
// violation. Emits LoggedEvent on success.
//
// `now` is the explicit timestamp — caller supplies a single `time.Time`
// per operation so all aggregates touched in one handler invocation
// share the same instant (audit consistency). No package-global clock
// dependency per the post-Wave-9 clock-injection refactor.
//
// Invariants:
//   - id + batchID + productID + tenantID + actorMembershipID non-zero
//   - Type is in the closed catalogue (batch.MovementType.IsValid())
//   - Inbound + Reservation + Release: Quantity > 0
//   - Outbound: Quantity < 0 (caller passes negative; ledger convention)
//   - Adjustment: Quantity != 0
//   - QuantityOnHandAfter >= 0
//   - Reason trimmed 1..500
//   - SourceReference, when supplied, <=200 chars
//
//nolint:gocyclo,cyclop // straight-line guard cascade per coding-standards "validation = sequential guard list"
func New(id ID, spec Spec, now time.Time) (*Movement, error) {
	if err := validateUUID("id", id.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("batch_id", spec.BatchID.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("product_id", spec.ProductID.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("tenant_id", spec.TenantID.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("actor_membership_id", spec.ActorMembershipID.String()); err != nil {
		return nil, err
	}
	if !spec.Type.IsValid() {
		return nil, fmt.Errorf("%w: unknown movement type %q", ErrInvalid, spec.Type)
	}
	if err := validateQuantityForType(spec.Type, spec.Quantity); err != nil {
		return nil, err
	}
	if spec.QuantityOnHandAfter < 0 {
		return nil, fmt.Errorf("%w: quantity_on_hand_after must be >= 0 (got %d)", ErrInvalid, spec.QuantityOnHandAfter)
	}
	reason := strings.TrimSpace(spec.Reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: reason required", ErrInvalid)
	}
	if len(reason) < ReasonMinLength || len(reason) > ReasonMaxLength {
		return nil, fmt.Errorf("%w: reason length %d not in [%d,%d]",
			ErrInvalid, len(reason), ReasonMinLength, ReasonMaxLength)
	}
	var sr *string
	if spec.SourceReference != nil {
		ref := strings.TrimSpace(*spec.SourceReference)
		if len(ref) > SourceReferenceMax {
			return nil, fmt.Errorf("%w: source_reference too long (max %d, got %d)",
				ErrInvalid, SourceReferenceMax, len(ref))
		}
		if ref != "" {
			sr = &ref
		}
	}
	now = now.UTC()
	m := &Movement{
		id:                  id,
		batchID:             spec.BatchID,
		productID:           spec.ProductID,
		tenantID:            spec.TenantID,
		movementType:        spec.Type,
		quantity:            spec.Quantity,
		quantityOnHandAfter: spec.QuantityOnHandAfter,
		reason:              reason,
		actorMembershipID:   spec.ActorMembershipID,
		sourceReference:     sr,
		occurredAt:          now,
	}
	m.recordEvent(LoggedEvent{
		MovementID:          id,
		BatchID:             spec.BatchID,
		ProductID:           spec.ProductID,
		TenantID:            spec.TenantID,
		Type:                spec.Type,
		Quantity:            spec.Quantity,
		QuantityOnHandAfter: spec.QuantityOnHandAfter,
		ActorMembershipID:   spec.ActorMembershipID,
		SourceReference:     sr,
		At:                  now,
	})
	return m, nil
}

// validateQuantityForType enforces the SIGNED-quantity convention per
// movement type. Encoded once here so the handler + adapter don't
// re-derive it.
func validateQuantityForType(mt batch.MovementType, q int64) error {
	switch mt {
	case batch.MovementInbound, batch.MovementReservation, batch.MovementRelease:
		if q <= 0 {
			return fmt.Errorf("%w: %s quantity must be > 0 (got %d)", ErrInvalid, mt, q)
		}
	case batch.MovementOutbound:
		if q >= 0 {
			return fmt.Errorf("%w: outbound quantity must be < 0 (got %d)", ErrInvalid, q)
		}
	case batch.MovementAdjustment:
		if q == 0 {
			return fmt.Errorf("%w: adjustment quantity must be non-zero", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown movement type %q", ErrInvalid, mt)
	}
	return nil
}

// ----- Getters --------------------------------------------------------------

// ID returns the movement's primary key.
func (m *Movement) ID() ID { return m.id }

// BatchID returns the FK to the affected batch.
func (m *Movement) BatchID() batch.ID { return m.batchID }

// ProductID returns the FK to the parent product (denormalised for ledger reads).
func (m *Movement) ProductID() product.ID { return m.productID }

// TenantID returns the FK to the tenant.
func (m *Movement) TenantID() tenant.ID { return m.tenantID }

// Type returns the movement classifier.
func (m *Movement) Type() batch.MovementType { return m.movementType }

// Quantity returns the signed delta (per SIGNED-convention; see package doc).
func (m *Movement) Quantity() int64 { return m.quantity }

// QuantityOnHandAfter returns the batch.quantity_on_hand AFTER applying
// this movement (snapshot for ledger views without re-query).
func (m *Movement) QuantityOnHandAfter() int64 { return m.quantityOnHandAfter }

// Reason returns the human-readable reason / note.
func (m *Movement) Reason() string { return m.reason }

// ActorMembershipID returns the membership that initiated the movement.
func (m *Movement) ActorMembershipID() membership.ID { return m.actorMembershipID }

// SourceReference returns the optional opaque identifier linking the
// movement to an external source (future OrderID, PurchaseOrderID,
// manual-adjustment ticket id). Nil when none.
func (m *Movement) SourceReference() *string { return m.sourceReference }

// OccurredAt returns the timestamp the movement was logged.
func (m *Movement) OccurredAt() time.Time { return m.occurredAt }

// ----- Persistence DTO ------------------------------------------------------

// Snapshot is the persistence-layer projection.
type Snapshot struct {
	ID                  ID
	BatchID             batch.ID
	ProductID           product.ID
	TenantID            tenant.ID
	Type                batch.MovementType
	Quantity            int64
	QuantityOnHandAfter int64
	Reason              string
	ActorMembershipID   membership.ID
	SourceReference     *string
	OccurredAt          time.Time
}

// UnmarshalFromDB re-hydrates from persistence. Repository-only.
// Does NOT re-validate. Does NOT emit events.
func UnmarshalFromDB(s Snapshot) *Movement {
	return &Movement{
		id:                  s.ID,
		batchID:             s.BatchID,
		productID:           s.ProductID,
		tenantID:            s.TenantID,
		movementType:        s.Type,
		quantity:            s.Quantity,
		quantityOnHandAfter: s.QuantityOnHandAfter,
		reason:              s.Reason,
		actorMembershipID:   s.ActorMembershipID,
		sourceReference:     s.SourceReference,
		occurredAt:          s.OccurredAt.UTC(),
	}
}

// ----- Event handling -------------------------------------------------------

// PullEvents drains the recorded events.
func (m *Movement) PullEvents() []Event {
	if len(m.events) == 0 {
		return nil
	}
	out := m.events
	m.events = nil
	return out
}

func (m *Movement) recordEvent(e Event) {
	m.events = append(m.events, e)
}

// ----- Listing --------------------------------------------------------------

// ListFilter narrows the per-batch ledger read endpoint
// (GET /v1/inventory/batches/{id}/movements?type=).
type ListFilter struct {
	// Type, when non-empty, restricts to movements of that type.
	Type batch.MovementType
}

// IsValid validates the filter shape. Empty Type means "all types"
// (no filter); a non-empty Type must be in the closed catalogue.
func (f ListFilter) IsValid() bool {
	if f.Type == "" {
		return true
	}
	return f.Type.IsValid()
}

// PageRequest is a tiny ergonomic struct for ListByBatchPage signatures.
// Kept here so the [Reader] signature line stays readable.
type PageRequest struct {
	Filter   ListFilter
	Cursor   pagination.Cursor
	PageSize int
}
