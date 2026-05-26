package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// ConsignmentNoteCreatedV1 — a pending ConsignmentNote slot is born.
// Typically driven by the OrderPacked subscriber (warehouse flipped
// Order to packed; Dispatch created the slot).
//
// Wire alias: `dispatch.consignment_note_created.v1`. Tenant-scoped.
type ConsignmentNoteCreatedV1 struct {
	ConsignmentNoteID     uuid.UUID  `json:"consignment_note_id"`
	TenantIDClaim         uuid.UUID  `json:"tenant_id"`
	OrderID               uuid.UUID  `json:"order_id"`
	CarrierName           string     `json:"carrier_name"`
	BoxCount              int32      `json:"box_count"`
	WeightGrams           int64      `json:"weight_grams"`
	ExpectedDeliveryAtUTC *time.Time `json:"expected_delivery_at_utc,omitzero"`
	CreatedByMembershipID uuid.UUID  `json:"created_by_membership_id"`
	OccurredAtUTC         time.Time  `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (ConsignmentNoteCreatedV1) Topic() string { return "dispatch.consignment_note_created.v1" }

// OccurredAt returns the domain timestamp.
func (e ConsignmentNoteCreatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e ConsignmentNoteCreatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// ConsignmentNoteDispatchedV1 — goods left the warehouse + the carrier-
// assigned docket number is recorded.
//
// Wire alias: `dispatch.consignment_note_dispatched.v1`.
type ConsignmentNoteDispatchedV1 struct {
	ConsignmentNoteID        uuid.UUID `json:"consignment_note_id"`
	TenantIDClaim            uuid.UUID `json:"tenant_id"`
	OrderID                  uuid.UUID `json:"order_id"`
	DocketNumber             string    `json:"docket_number"`
	TransitionedByMembership uuid.UUID `json:"transitioned_by_membership"`
	OccurredAtUTC            time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (ConsignmentNoteDispatchedV1) Topic() string {
	return "dispatch.consignment_note_dispatched.v1"
}

// OccurredAt returns the domain timestamp.
func (e ConsignmentNoteDispatchedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e ConsignmentNoteDispatchedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// ConsignmentNoteInTransitV1 — first carrier in-transit scan event.
// Driven by carrier-webhook subscriber.
//
// Wire alias: `dispatch.consignment_note_in_transit.v1`.
type ConsignmentNoteInTransitV1 struct {
	ConsignmentNoteID        uuid.UUID `json:"consignment_note_id"`
	TenantIDClaim            uuid.UUID `json:"tenant_id"`
	OrderID                  uuid.UUID `json:"order_id"`
	TransitionedByMembership uuid.UUID `json:"transitioned_by_membership"`
	OccurredAtUTC            time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (ConsignmentNoteInTransitV1) Topic() string {
	return "dispatch.consignment_note_in_transit.v1"
}

// OccurredAt returns the domain timestamp.
func (e ConsignmentNoteInTransitV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e ConsignmentNoteInTransitV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// ConsignmentDeliveredV1 — terminal-success transition. Per ADR 0063
// §4 fulfillment saga: Orders module's subscriber consumes this +
// transitions Order to delivered. Notifications consumes this to
// fan out "your order arrived" alerts.
//
// Note the SHORTER alias (no "_note" prefix) — this is the saga
// input event; subscribers route on alias and the shorter form is
// what callers grep for.
//
// Wire alias: `dispatch.consignment_delivered.v1`.
type ConsignmentDeliveredV1 struct {
	ConsignmentNoteID        uuid.UUID `json:"consignment_note_id"`
	TenantIDClaim            uuid.UUID `json:"tenant_id"`
	OrderID                  uuid.UUID `json:"order_id"`
	DeliveredAtUTC           time.Time `json:"delivered_at_utc"`
	TransitionedByMembership uuid.UUID `json:"transitioned_by_membership"`
	OccurredAtUTC            time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (ConsignmentDeliveredV1) Topic() string { return "dispatch.consignment_delivered.v1" }

// OccurredAt returns the domain timestamp.
func (e ConsignmentDeliveredV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e ConsignmentDeliveredV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// ConsignmentNoteFailedV1 — terminal-failure transition. Reason is
// carrier-supplied. Operator manually initiates RTO via separate flow.
//
// Wire alias: `dispatch.consignment_note_failed.v1`.
type ConsignmentNoteFailedV1 struct {
	ConsignmentNoteID        uuid.UUID `json:"consignment_note_id"`
	TenantIDClaim            uuid.UUID `json:"tenant_id"`
	OrderID                  uuid.UUID `json:"order_id"`
	Reason                   string    `json:"reason"`
	FailedAtUTC              time.Time `json:"failed_at_utc"`
	TransitionedByMembership uuid.UUID `json:"transitioned_by_membership"`
	OccurredAtUTC            time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (ConsignmentNoteFailedV1) Topic() string { return "dispatch.consignment_note_failed.v1" }

// OccurredAt returns the domain timestamp.
func (e ConsignmentNoteFailedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e ConsignmentNoteFailedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Compile-time assertions + catalogue registration for the arch test.
var (
	_ TenantScoped = ConsignmentNoteCreatedV1{}
	_ TenantScoped = ConsignmentNoteDispatchedV1{}
	_ TenantScoped = ConsignmentNoteInTransitV1{}
	_ TenantScoped = ConsignmentDeliveredV1{}
	_ TenantScoped = ConsignmentNoteFailedV1{}

	_ = register(ConsignmentNoteCreatedV1{})
	_ = register(ConsignmentNoteDispatchedV1{})
	_ = register(ConsignmentNoteInTransitV1{})
	_ = register(ConsignmentDeliveredV1{})
	_ = register(ConsignmentNoteFailedV1{})
)
