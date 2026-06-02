// Package consignmentnote owns the [ConsignmentNote] aggregate — the
// transport document that travels with goods from warehouse to
// consignee. Per BRD §6.6 (formal) + §4.8 (informal "builty").
//
// Lifecycle:
//
//	pending → dispatched → in_transit → delivered | failed
//
// Created by a subscriber on `orders.order_packed.v1` (warehouse
// flips Order to packed; Dispatch creates a pending ConsignmentNote
// slot). Carrier-webhook updates flip the status; the terminal-success
// path publishes `dispatch.consignment_delivered.v1` so the Orders
// module's subscriber can transition Order to delivered (ADR 0063 §4
// fulfillment saga).
//
// Cross-module reference discipline (ADR 0001 modular monolith):
// OrderID is a STRING-typed alias, NOT an import of
// `internal/orders/domain/order`. The wire-stable UUID is the
// inter-module contract; the Order aggregate's invariants are NOT
// re-entered into Dispatch. The composite FK at the DB layer
// references orders.orders(tenant_id, id) directly.
package consignmentnote

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrInvalid is the sentinel for ctor / mutator invariant violations.
// Map to HTTP 422.
var ErrInvalid = errors.New("consignmentnote: invalid")

// ErrInvalidTransition — illegal (cur, target) status edge. Map to 409.
var ErrInvalidTransition = errors.New("consignmentnote: invalid status transition")

// ID is a UUIDv7.
type ID string

// IsZero reports whether id is unset.
func (id ID) IsZero() bool { return id == "" }

// String returns the underlying UUID string.
func (id ID) String() string { return string(id) }

// OrderID is the cross-module reference to `orders.orders(id)`. Modelled
// as a local STRING-typed alias rather than importing the order package
// — the modular monolith doctrine forbids domain-domain imports; the
// wire-stable UUID is the inter-module contract.
type OrderID string

// IsZero reports whether o is unset.
func (o OrderID) IsZero() bool { return o == "" }

// String returns the underlying UUID string.
func (o OrderID) String() string { return string(o) }

// ConsignmentNote is the aggregate root. One row per shipment in
// `dispatch.consignment_notes`.
type ConsignmentNote struct {
	id                    ID
	tenantID              tenant.ID
	orderID               OrderID
	status                Status
	carrierName           string
	docketNumber          string
	boxCount              int32
	weightGrams           int64
	expectedDeliveryAt    *time.Time
	dispatchedAt          *time.Time
	inTransitAt           *time.Time
	deliveredAt           *time.Time
	failedAt              *time.Time
	failureReason         string
	createdAt             time.Time
	createdByMembershipID membership.ID

	events []Event
}

// NewInput is the ctor input. The aggregate starts in `pending` —
// docket number + dispatch timestamp arrive later via mutators.
type NewInput struct {
	ID                    ID
	TenantID              tenant.ID
	OrderID               OrderID
	CarrierName           string
	BoxCount              int32
	WeightGrams           int64
	ExpectedDeliveryAt    *time.Time
	CreatedByMembershipID membership.ID
	Now                   time.Time
}

// New constructs a pending ConsignmentNote.
func New(in NewInput) (*ConsignmentNote, error) {
	if err := validateUUID("id", in.ID.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("tenant_id", in.TenantID.String()); err != nil {
		return nil, err
	}
	if err := validateUUID("order_id", in.OrderID.String()); err != nil {
		return nil, err
	}
	carrier := strings.TrimSpace(in.CarrierName)
	if carrier == "" {
		return nil, fmt.Errorf("%w: carrier_name required", ErrInvalid)
	}
	if in.BoxCount <= 0 {
		return nil, fmt.Errorf("%w: box_count must be positive (got %d)", ErrInvalid, in.BoxCount)
	}
	if in.WeightGrams <= 0 {
		return nil, fmt.Errorf("%w: weight_grams must be positive (got %d)", ErrInvalid, in.WeightGrams)
	}
	if err := validateUUID("created_by_membership_id", in.CreatedByMembershipID.String()); err != nil {
		return nil, err
	}
	if in.Now.IsZero() {
		return nil, fmt.Errorf("%w: now required", ErrInvalid)
	}
	if in.ExpectedDeliveryAt != nil && in.ExpectedDeliveryAt.Before(in.Now) {
		return nil, fmt.Errorf("%w: expected_delivery_at must be in the future", ErrInvalid)
	}
	cn := &ConsignmentNote{
		id:                    in.ID,
		tenantID:              in.TenantID,
		orderID:               in.OrderID,
		status:                StatusPending,
		carrierName:           carrier,
		boxCount:              in.BoxCount,
		weightGrams:           in.WeightGrams,
		expectedDeliveryAt:    in.ExpectedDeliveryAt,
		createdAt:             in.Now,
		createdByMembershipID: in.CreatedByMembershipID,
	}
	cn.recordEvent(CreatedEvent{
		ConsignmentNoteID:     cn.id,
		TenantID:              cn.tenantID,
		OrderID:               cn.orderID,
		CarrierName:           cn.carrierName,
		BoxCount:              cn.boxCount,
		WeightGrams:           cn.weightGrams,
		ExpectedDeliveryAt:    cn.expectedDeliveryAt,
		CreatedAt:             cn.createdAt,
		CreatedByMembershipID: cn.createdByMembershipID,
	})
	return cn, nil
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

// Snapshot is the persistence DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	ID                    ID
	TenantID              tenant.ID
	OrderID               OrderID
	Status                Status
	CarrierName           string
	DocketNumber          string
	BoxCount              int32
	WeightGrams           int64
	ExpectedDeliveryAt    *time.Time
	DispatchedAt          *time.Time
	InTransitAt           *time.Time
	DeliveredAt           *time.Time
	FailedAt              *time.Time
	FailureReason         string
	CreatedAt             time.Time
	CreatedByMembershipID membership.ID
}

// UnmarshalFromDB rehydrates without re-validating.
func UnmarshalFromDB(s Snapshot) *ConsignmentNote {
	return &ConsignmentNote{
		id:                    s.ID,
		tenantID:              s.TenantID,
		orderID:               s.OrderID,
		status:                s.Status,
		carrierName:           s.CarrierName,
		docketNumber:          s.DocketNumber,
		boxCount:              s.BoxCount,
		weightGrams:           s.WeightGrams,
		expectedDeliveryAt:    s.ExpectedDeliveryAt,
		dispatchedAt:          s.DispatchedAt,
		inTransitAt:           s.InTransitAt,
		deliveredAt:           s.DeliveredAt,
		failedAt:              s.FailedAt,
		failureReason:         s.FailureReason,
		createdAt:             s.CreatedAt,
		createdByMembershipID: s.CreatedByMembershipID,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the aggregate identity.
func (cn *ConsignmentNote) ID() ID { return cn.id }

// TenantID returns the owning tenant.
func (cn *ConsignmentNote) TenantID() tenant.ID { return cn.tenantID }

// OrderID returns the linked order.
func (cn *ConsignmentNote) OrderID() OrderID { return cn.orderID }

// Status returns the current lifecycle status.
func (cn *ConsignmentNote) Status() Status { return cn.status }

// CarrierName returns the carrier identifier (e.g. "BlueDart").
func (cn *ConsignmentNote) CarrierName() string { return cn.carrierName }

// DocketNumber returns the carrier-assigned tracking number; empty
// when still pending.
func (cn *ConsignmentNote) DocketNumber() string { return cn.docketNumber }

// BoxCount returns the shipment box count.
func (cn *ConsignmentNote) BoxCount() int32 { return cn.boxCount }

// WeightGrams returns the shipment weight.
func (cn *ConsignmentNote) WeightGrams() int64 { return cn.weightGrams }

// ExpectedDeliveryAt returns the carrier's promised delivery date.
func (cn *ConsignmentNote) ExpectedDeliveryAt() *time.Time { return cn.expectedDeliveryAt }

// DispatchedAt returns when goods left the warehouse.
func (cn *ConsignmentNote) DispatchedAt() *time.Time { return cn.dispatchedAt }

// InTransitAt returns the first carrier in-transit scan timestamp.
func (cn *ConsignmentNote) InTransitAt() *time.Time { return cn.inTransitAt }

// DeliveredAt returns the delivery confirmation timestamp.
func (cn *ConsignmentNote) DeliveredAt() *time.Time { return cn.deliveredAt }

// FailedAt returns the failure timestamp.
func (cn *ConsignmentNote) FailedAt() *time.Time { return cn.failedAt }

// FailureReason returns the carrier-supplied failure reason.
func (cn *ConsignmentNote) FailureReason() string { return cn.failureReason }

// CreatedAt returns the row-creation timestamp.
func (cn *ConsignmentNote) CreatedAt() time.Time { return cn.createdAt }

// CreatedByMembershipID returns the actor.
func (cn *ConsignmentNote) CreatedByMembershipID() membership.ID { return cn.createdByMembershipID }

// ----- State transitions ----------------------------------------------------

// MarkDispatched flips pending → dispatched, recording the docket
// number assigned by the carrier. docketNumber is required (the
// pending → dispatched transition exists to capture this docket).
// Idempotent on self when the docketNumber matches; differing docket
// on re-call is ErrInvalid (operator typo or wrong carrier).
func (cn *ConsignmentNote) MarkDispatched(docketNumber string, actor membership.ID, now time.Time) error {
	doc := strings.TrimSpace(docketNumber)
	if doc == "" {
		return fmt.Errorf("%w: docket_number required", ErrInvalid)
	}
	if cn.status == StatusDispatched {
		if cn.docketNumber != doc {
			return fmt.Errorf("%w: docket_number mismatch on re-mark (existing %q, supplied %q)",
				ErrInvalid, cn.docketNumber, doc)
		}
		return nil // idempotent
	}
	if err := cn.advance(StatusDispatched, actor, now); err != nil {
		return err
	}
	cn.docketNumber = doc
	if cn.dispatchedAt == nil {
		cn.dispatchedAt = &now
	}
	return nil
}

// MarkInTransit flips dispatched → in_transit. Typically driven by
// the carrier-webhook subscriber.
func (cn *ConsignmentNote) MarkInTransit(actor membership.ID, now time.Time) error {
	if err := cn.advance(StatusInTransit, actor, now); err != nil {
		return err
	}
	if cn.inTransitAt == nil {
		cn.inTransitAt = &now
	}
	return nil
}

// MarkDelivered transitions to the terminal-success state. Publishes
// DeliveredEvent → Orders subscriber transitions Order to delivered.
// Acceptable from dispatched OR in_transit (some carriers skip the
// in-transit scan + jump straight from dispatched → delivered).
func (cn *ConsignmentNote) MarkDelivered(actor membership.ID, now time.Time) error {
	if err := cn.advance(StatusDelivered, actor, now); err != nil {
		return err
	}
	if cn.deliveredAt == nil {
		cn.deliveredAt = &now
	}
	return nil
}

// MarkFailed transitions to the terminal-failure state with a
// carrier-supplied reason. Acceptable from pending / dispatched /
// in_transit (any non-terminal state).
func (cn *ConsignmentNote) MarkFailed(reason string, actor membership.ID, now time.Time) error {
	r := strings.TrimSpace(reason)
	if r == "" {
		return fmt.Errorf("%w: failure reason required", ErrInvalid)
	}
	if err := cn.advance(StatusFailed, actor, now); err != nil {
		return err
	}
	cn.failureReason = r
	if cn.failedAt == nil {
		cn.failedAt = &now
	}
	return nil
}

// advance is the shared transition primitive: idempotent on self,
// terminal-guard, canAdvance check, event emission.
func (cn *ConsignmentNote) advance(target Status, actor membership.ID, now time.Time) error {
	if cn.status == target {
		return nil
	}
	if cn.status.IsTerminal() {
		return fmt.Errorf("%w: cannot transition from terminal status %s", ErrInvalidTransition, cn.status)
	}
	if !canAdvance(cn.status, target) {
		return fmt.Errorf("%w: %s → %s not permitted", ErrInvalidTransition, cn.status, target)
	}
	if actor == "" {
		return fmt.Errorf("%w: actor required", ErrInvalid)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	prior := cn.status
	cn.status = target
	cn.recordEvent(StatusChangedEvent{
		ConsignmentNoteID:        cn.id,
		TenantID:                 cn.tenantID,
		OrderID:                  cn.orderID,
		PriorStatus:              prior,
		NewStatus:                target,
		TransitionedAt:           now,
		TransitionedByMembership: actor,
	})
	return nil
}

// ----- Events --------------------------------------------------------------

// PullEvents drains the recorded domain events.
func (cn *ConsignmentNote) PullEvents() []Event {
	if len(cn.events) == 0 {
		return nil
	}
	out := cn.events
	cn.events = nil
	return out
}

func (cn *ConsignmentNote) recordEvent(e Event) { cn.events = append(cn.events, e) }
