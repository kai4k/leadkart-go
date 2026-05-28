package consignmentnote

import "fmt"

// Status is the lifecycle position of a [ConsignmentNote] per BRD §6.6.
//
// State machine (strict):
//
//	pending → dispatched → in_transit → delivered (terminal-success)
//	                                  ↘ failed    (terminal-failure)
//
// Self-transitions are idempotent. Terminal states refuse outgoing
// transitions.
type Status string

// Closed catalogue. Wire-stable lowercase strings — match the CHECK
// constraint on dispatch.consignment_notes.status in the init
// migration.
const (
	// StatusPending — note created (slot reserved) but not yet handed
	// to carrier. The carrier docket number may be unknown at this
	// stage; the warehouse fills it in before flipping to dispatched.
	StatusPending Status = "pending"

	// StatusDispatched — note handed to carrier; docket number
	// confirmed; in carrier's possession.
	StatusDispatched Status = "dispatched"

	// StatusInTransit — carrier scan event confirms goods are moving.
	// Typically driven by carrier webhook.
	StatusInTransit Status = "in_transit"

	// StatusDelivered — carrier confirms delivery to consignee.
	// Terminal-success. Publishes `dispatch.consignment_delivered.v1`
	// — the Orders module's subscriber transitions Order to delivered.
	StatusDelivered Status = "delivered"

	// StatusFailed — carrier reports undeliverable (consignee absent,
	// address invalid, refused). Terminal-failure. Operator manually
	// initiates RTO (return-to-origin) via separate flow.
	StatusFailed Status = "failed"
)

// String returns the wire form.
func (s Status) String() string { return string(s) }

// IsTerminal reports whether the status allows NO further transitions.
func (s Status) IsTerminal() bool { return s == StatusDelivered || s == StatusFailed }

// IsValid reports whether s is a known catalogue entry.
func (s Status) IsValid() bool {
	switch s {
	case StatusPending, StatusDispatched, StatusInTransit, StatusDelivered, StatusFailed:
		return true
	}
	return false
}

// ParseStatus turns an untrusted string into a [Status] or returns
// [ErrInvalid] wrapped with the bad value.
func ParseStatus(raw string) (Status, error) {
	s := Status(raw)
	if !s.IsValid() {
		return "", fmt.Errorf("%w: status %q not in catalogue", ErrInvalid, raw)
	}
	return s, nil
}

// canAdvance reports whether cur → target is a permitted forward edge.
// Self-transitions handled BEFORE this function runs.
func canAdvance(cur, target Status) bool {
	switch cur {
	case StatusPending:
		return target == StatusDispatched || target == StatusFailed
	case StatusDispatched:
		return target == StatusInTransit || target == StatusDelivered || target == StatusFailed
	case StatusInTransit:
		return target == StatusDelivered || target == StatusFailed
	}
	return false
}
