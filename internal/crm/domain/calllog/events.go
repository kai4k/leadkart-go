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

// LoggedEvent fires when a CallLog is created via [New]. The repository
// drains + maps to crm.call_logged.v1.
type LoggedEvent struct {
	CallID               ID
	LeadID               crmlead.ID
	TenantID             tenant.ID
	Outcome              Outcome
	LoggedByMembershipID string
	At                   time.Time
}

func (LoggedEvent) isCallLogEvent() {}
