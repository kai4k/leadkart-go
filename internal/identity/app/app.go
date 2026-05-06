// Package app holds the Identity Application facade.
//
// Per TDL Wild Workouts canonical layout: an Application{Commands,
// Queries} struct aggregates concrete handler structs as fields. HTTP
// + future gRPC + future CLI ports call `app.Commands.X.Handle(...)`
// directly — no service interface, no mediator.
//
// Composition root (cmd/api/main.go) builds an Application by wiring
// concrete adapters into each handler's constructor + assembling the
// facade. Tests typically construct a partial Application with fakes
// for the handlers under test.
package app

import (
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
)

// Application is the Identity facade. Every external port (HTTP,
// future gRPC, event subscribers) takes an Application and dispatches
// directly into its handler fields.
type Application struct {
	Commands Commands
	// Queries Queries — added when query handlers land
	// (GetTenant, ListMemberships, etc.); not part of the Week-5 cut.
}

// Commands aggregates all Identity command handlers. One field per
// use case. New use cases extend this struct, never reach into a
// shared service abstraction.
type Commands struct {
	RegisterTenant command.RegisterTenantHandler
	Login          command.LoginHandler
	Refresh        command.RefreshHandler
	Logout         command.LogoutHandler
	ChangePassword command.ChangePasswordHandler
}
