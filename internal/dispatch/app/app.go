// Package app holds the Dispatch module Application facade.
//
// Per TDL Wild Workouts canonical layout: an Application{Commands,
// Queries} struct aggregates concrete handler structs as fields. HTTP
// + event subscribers call `app.Commands.X.Handle(...)` directly —
// no service interface, no mediator.
//
// Composition root (cmd/api/main.go + cmd/worker/main.go) builds an
// Application by wiring concrete adapters into each handler's
// constructor + assembling the facade.
package app

import "github.com/leadkart/leadkart-go/internal/dispatch/app/command"

// Application is the Dispatch facade. Every external port (HTTP +
// subscribers) takes an Application + dispatches directly into its
// handler fields.
type Application struct {
	Commands Commands
}

// Commands aggregates all Dispatch command handlers.
type Commands struct {
	CreateConsignmentNote command.CreateConsignmentNoteHandler
	MarkDispatched        command.MarkDispatchedHandler
	MarkInTransit         command.MarkInTransitHandler
	MarkDelivered         command.MarkDeliveredHandler
	MarkFailed            command.MarkFailedHandler
}
