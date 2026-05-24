// Package app holds the Platform module Application facade.
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
	"github.com/leadkart/leadkart-go/internal/platform/app/command"
	"github.com/leadkart/leadkart-go/internal/platform/app/query"
)

// Application is the Platform facade. Every external port (HTTP,
// future gRPC, event subscribers) takes an Application + dispatches
// directly into its handler fields.
type Application struct {
	Commands Commands
	Queries  Queries
}

// Commands aggregates all Platform command handlers. One field per
// use case. New use cases extend this struct.
type Commands struct {
	CreateUnverifiedContact command.CreateUnverifiedContactHandler
	LogVerificationCall     command.LogVerificationCallHandler
	VerifyUnverifiedContact command.VerifyUnverifiedContactHandler
	RejectUnverifiedContact command.RejectUnverifiedContactHandler
	PurchaseLead            command.PurchaseLeadHandler
	TopupLeadCredits        command.TopupLeadCreditsHandler
}

// Queries aggregates all Platform query handlers. Read-side only.
type Queries struct {
	ListUnverifiedContacts query.ListUnverifiedContactsHandler
	BrowseMarketplace      query.BrowseMarketplaceHandler
	GetLeadCreditBalance   query.GetLeadCreditBalanceHandler
}
