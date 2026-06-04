package calllog

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the SEALED marker interface for CallLog domain events.
type Event interface {
	isCallLogEvent()
}

// LoggedEvent fires when a CallLog is created via [New] /
// [NewWithCallback]. The repository drains + maps to crm.call_logged.v1.
//
// CallbackWindowStart + CallbackWindowEnd are populated when the caller
// stamped a callback window per BRD §4.5. Both zero when the call did
// not request a callback. The Reminder slice's CallLogged subscriber
// short-circuits when both are zero; otherwise it mints a callback
// reminder due at Start.
type LoggedEvent struct {
	CallID               ID
	LeadID               crmlead.ID
	TenantID             tenant.ID
	Outcome              Outcome
	LoggedByMembershipID string
	CallbackWindowStart  time.Time
	CallbackWindowEnd    time.Time
	At                   time.Time
}

func (LoggedEvent) isCallLogEvent() {}
