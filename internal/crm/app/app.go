// Package app holds the CRM Application facade.
//
// Per TDL Wild Workouts canonical layout: an Application{Commands,
// Queries} struct aggregates concrete handler structs as fields. HTTP +
// future gRPC + future CLI ports call `app.Commands.X.Handle(...)` /
// `app.Queries.Y.Handle(...)` directly — no service interface, no
// mediator.
//
// Composition root (cmd/api/main.go) builds the Application by wiring
// concrete adapters into each handler's constructor + assembling the
// facade. Tests typically construct a partial Application with fakes
// for the handlers under test.
package app

import (
	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/app/query"
)

// Application is the CRM facade. Every external port (HTTP, future
// gRPC, event subscribers) takes an Application and dispatches directly
// into its handler fields.
type Application struct {
	Commands Commands
	Queries  Queries
}

// Commands aggregates all CRM command handlers. One field per use case.
// New use cases extend this struct, never reach into a shared service
// abstraction.
type Commands struct {
	IngestPurchasedLead   command.IngestPurchasedLeadHandler
	AssignLead            command.AssignLeadHandler
	ChangeLeadStage       command.ChangeLeadStageHandler
	ChangeLeadTemperature command.ChangeLeadTemperatureHandler
	LogCall               command.LogCallHandler
	ConvertLead           command.ConvertLeadHandler
	LoseLead              command.LoseLeadHandler
	CreateReminder        command.CreateReminderHandler
	MarkReminderSent      command.MarkReminderSentHandler
	CancelReminder        command.CancelReminderHandler
}

// Queries aggregates all CRM query handlers. Read-side only — no state
// mutation.
type Queries struct {
	GetLead              query.GetLeadHandler
	ListLeads            query.ListLeadsHandler
	ListPendingReminders query.ListPendingRemindersHandler
}
