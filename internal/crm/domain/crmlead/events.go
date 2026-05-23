package crmlead

import "time"

// Event is the SEALED marker interface for CrmLead domain events.
// Sealed via the unexported isCrmLeadEvent() method so only types in
// this package can satisfy it — same shape as identity domain events.
//
// Domain events deliberately do NOT carry wire concerns (Topic / V1
// alias / occurred-at-as-method). Wire-versioning lives in
// internal/crm/integrationevents/*V1 per Vernon IDDD ch.8 — a v2 wire
// rename must NOT force a domain edit. The integration mapper in
// internal/crm/integrationevents/ type-switches on these structs and
// emits the canonical V1 envelope.
type Event interface {
	isCrmLeadEvent()
}

// CreatedEvent fires when a CrmLead is created via [New] or
// [NewFromPurchaseSnapshot]. SourcePurchaseID is empty for manual-import
// leads, populated for subscriber-created leads.
type CreatedEvent struct {
	LeadID                ID
	TenantID              string
	SourcePurchaseID      string // empty for manual-import
	CreatedByMembershipID string
	At                    time.Time
}

func (CreatedEvent) isCrmLeadEvent() {}

// AssignedEvent fires when [Assign] changes the current assignee.
// PreviousAssignee is empty for the first assignment.
type AssignedEvent struct {
	LeadID                 ID
	TenantID               string
	PreviousAssignee       string // empty for first assignment
	AssigneeMembershipID   string
	AssignedByMembershipID string
	Reason                 string
	At                     time.Time
}

func (AssignedEvent) isCrmLeadEvent() {}

// StageChangedEvent fires when [ChangeStage] advances the stage.
// Convert / Lose use [ConvertedEvent] / [LostEvent] instead.
type StageChangedEvent struct {
	LeadID                ID
	TenantID              string
	OldStage              Stage
	NewStage              Stage
	ChangedByMembershipID string
	Reason                string
	At                    time.Time
}

func (StageChangedEvent) isCrmLeadEvent() {}

// TemperatureChangedEvent fires when [ChangeTemperature] updates the
// temperature axis.
type TemperatureChangedEvent struct {
	LeadID                ID
	TenantID              string
	OldTemperature        Temperature
	NewTemperature        Temperature
	ChangedByMembershipID string
	At                    time.Time
}

func (TemperatureChangedEvent) isCrmLeadEvent() {}

// ConvertedEvent fires when [Convert] terminally closes the lead as
// won. The future Orders module consumes this as the trigger to create
// a Quotation skeleton (per ADR 0060 contract for the v0.4+ Orders work).
type ConvertedEvent struct {
	LeadID                  ID
	TenantID                string
	ConvertedByMembershipID string
	At                      time.Time
}

func (ConvertedEvent) isCrmLeadEvent() {}

// LostEvent fires when [Lose] terminally closes the lead as lost.
// Reason MUST be non-empty (audit doctrine).
type LostEvent struct {
	LeadID             ID
	TenantID           string
	LostByMembershipID string
	Reason             string
	At                 time.Time
}

func (LostEvent) isCrmLeadEvent() {}
