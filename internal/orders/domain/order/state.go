package order

import "fmt"

// State is an [Order]'s position in the fulfillment lifecycle (BRD §6.4, ADR 0063).
//
// Strict machine, no skips/backtracking:
//
//	quotation_draft → quotation_approved → token_paid → confirmed →
//	  packed → invoiced → dispatched → delivered → complete (terminal)
//	(any non-terminal) → cancelled (terminal)
//
// Terminal states refuse transitions; self-transitions are idempotent no-ops.
// Cancellation is reachable from any non-terminal state; per-state compensation
// (unreserve stock, mint credit note, cancel consignment) fires via subscribers
// on the orders.order_cancelled.v1 envelope — see ADR 0063 §4.
type State string

// Closed catalogue. Wire-stable lowercase strings — must match the CHECK
// constraint on orders.orders.state in the init migration.
const (
	StateQuotationDraft    State = "quotation_draft"
	StateQuotationApproved State = "quotation_approved"
	StateTokenPaid         State = "token_paid"
	StateConfirmed         State = "confirmed"
	StatePacked            State = "packed"
	StateInvoiced          State = "invoiced"
	StateDispatched        State = "dispatched"
	StateDelivered         State = "delivered"
	StateComplete          State = "complete"
	StateCancelled         State = "cancelled"
)

// String returns the wire form.
func (s State) String() string { return string(s) }

// IsTerminal reports whether the state allows NO further transitions.
func (s State) IsTerminal() bool { return s == StateComplete || s == StateCancelled }

// IsValid reports whether s is a known catalogue entry. Used at the HTTP edge
// to reject untrusted input before it reaches the aggregate.
func (s State) IsValid() bool {
	switch s {
	case StateQuotationDraft, StateQuotationApproved, StateTokenPaid,
		StateConfirmed, StatePacked, StateInvoiced,
		StateDispatched, StateDelivered, StateComplete, StateCancelled:
		return true
	}
	return false
}

// ParseState converts an untrusted string to a [State], wrapping [ErrInvalid]
// with the bad value on failure.
func ParseState(raw string) (State, error) {
	s := State(raw)
	if !s.IsValid() {
		return "", fmt.Errorf("%w: state %q not in catalogue", ErrInvalid, raw)
	}
	return s, nil
}

// canAdvance reports whether cur → target is a permitted forward edge.
// Cancellation has its own mutator; self-transitions are handled before this runs.
func canAdvance(cur, target State) bool {
	switch cur {
	case StateQuotationDraft:
		return target == StateQuotationApproved
	case StateQuotationApproved:
		return target == StateTokenPaid
	case StateTokenPaid:
		return target == StateConfirmed
	case StateConfirmed:
		return target == StatePacked
	case StatePacked:
		return target == StateInvoiced
	case StateInvoiced:
		return target == StateDispatched
	case StateDispatched:
		return target == StateDelivered
	case StateDelivered:
		return target == StateComplete
	}
	return false
}
