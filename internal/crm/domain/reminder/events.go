package reminder

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the SEALED marker interface for Reminder domain events.
// Sealed via the unexported [isReminderEvent] method so only types in
// this package can satisfy it — same shape as the other CRM aggregates.
//
// Domain events deliberately do NOT carry wire concerns (Topic / V1
// alias / occurred-at-as-method). Wire-versioning lives in
// internal/crm/integrationevents/*V1 — a v2 wire rename must NOT force
// a domain edit. The integration mapper type-switches on these structs
// and emits the canonical V1 envelope.
type Event interface {
	isReminderEvent()
}

// CreatedEvent fires when a Reminder is constructed via one of the
// factory entry points ([NewCallbackReminder] / [NewMatureLeadReminder]
// / [NewManualReminder]). SourceCallLogID is empty for non-callback
// types.
type CreatedEvent struct {
	ReminderID            ID
	TenantID              tenant.ID
	LeadID                crmlead.ID
	AssignedToMembershipID string
	Type                  Type
	DueAt                 time.Time
	SourceCallLogID       string // empty for non-callback reminders
	CreatedByMembershipID string // empty for subscriber/cron-created reminders
	At                    time.Time
}

func (CreatedEvent) isReminderEvent() {}

// MarkedSentEvent fires when [Reminder.MarkSent] flips state to
// [StateSent]. Terminal — no further events from this reminder.
type MarkedSentEvent struct {
	ReminderID          ID
	TenantID            tenant.ID
	LeadID              crmlead.ID
	MarkedByMembershipID string
	At                  time.Time
}

func (MarkedSentEvent) isReminderEvent() {}

// CancelledEvent fires when [Reminder.Cancel] flips state to
// [StateCancelled]. Carries the audit reason — required by the cancel
// mutator.
type CancelledEvent struct {
	ReminderID              ID
	TenantID                tenant.ID
	LeadID                  crmlead.ID
	CancelledByMembershipID string
	Reason                  string
	At                      time.Time
}

func (CancelledEvent) isReminderEvent() {}
