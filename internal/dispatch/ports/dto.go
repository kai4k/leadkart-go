// Package ports holds the Dispatch module's inbound HTTP + subscriber
// adapters per ADR 0002 + the TDL hexagonal terminology canon.
//
// HTTP handlers translate request/response shapes into [app.Application]
// command + query calls. Per ADR 0046 the wire shapes here are MIRRORED
// in api/openapi.yaml — adding a route or field requires a paired spec
// update (CI-gated by the route-spec-drift test).
package ports

import "time"

// ----- Consignment-note read DTO -------------------------------------------

// ConsignmentNoteDto is the wire shape returned by the read endpoints.
type ConsignmentNoteDto struct {
	ID                    string     `json:"id"`
	TenantID              string     `json:"tenant_id"`
	OrderID               string     `json:"order_id"`
	Status                string     `json:"status"`
	CarrierName           string     `json:"carrier_name"`
	DocketNumber          string     `json:"docket_number,omitzero"`
	BoxCount              int32      `json:"box_count"`
	WeightGrams           int64      `json:"weight_grams"`
	ExpectedDeliveryAt    *time.Time `json:"expected_delivery_at,omitzero"`
	DispatchedAt          *time.Time `json:"dispatched_at,omitzero"`
	InTransitAt           *time.Time `json:"in_transit_at,omitzero"`
	DeliveredAt           *time.Time `json:"delivered_at,omitzero"`
	FailedAt              *time.Time `json:"failed_at,omitzero"`
	FailureReason         string     `json:"failure_reason,omitzero"`
	CreatedAt             time.Time  `json:"created_at"`
	CreatedByMembershipID string     `json:"created_by_membership_id"`
}

// ----- Mutation requests + responses ---------------------------------------

// CreateConsignmentNoteRequest is the body for POST /consignment-notes.
type CreateConsignmentNoteRequest struct {
	OrderID            string     `json:"order_id"`
	CarrierName        string     `json:"carrier_name"`
	BoxCount           int32      `json:"box_count"`
	WeightGrams        int64      `json:"weight_grams"`
	ExpectedDeliveryAt *time.Time `json:"expected_delivery_at,omitzero"`
}

// CreateConsignmentNoteResponse returns the new (or pre-existing) note ID.
type CreateConsignmentNoteResponse struct {
	ConsignmentNoteID string `json:"consignment_note_id"`
	AlreadyExisted    bool   `json:"already_existed"`
}

// MarkDispatchedRequest is the body for POST /{id}/dispatch — the carrier
// docket number assigned when goods are handed over.
type MarkDispatchedRequest struct {
	DocketNumber string `json:"docket_number"`
}

// MarkFailedRequest is the body for POST /{id}/failed.
type MarkFailedRequest struct {
	Reason string `json:"reason"`
}

// errorResponse is the wire shape for non-success responses.
type errorResponse struct {
	Type    string              `json:"type,omitzero"`
	Title   string              `json:"title,omitzero"`
	Status  int                 `json:"status,omitzero"`
	Detail  string              `json:"detail,omitzero"`
	Error   string              `json:"error"`
	Message string              `json:"message,omitzero"`
	Errors  map[string][]string `json:"errors,omitzero"`
}
